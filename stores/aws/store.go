package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"excalidraw-complete/core"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/oklog/ulid/v2"
)

type s3Store struct {
	s3Client *s3.Client
	bucket   string
}

// NewStore creates a new S3-based store.
func NewStore(bucketName string) *s3Store {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatalf("unable to load SDK config, %v", err)
	}

	s3Client := s3.NewFromConfig(cfg)

	return &s3Store{
		s3Client: s3Client,
		bucket:   bucketName,
	}
}

// DocumentStore implementation for anonymous sharing
func (s *s3Store) FindID(ctx context.Context, id string) (*core.Document, error) {
	resp, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(id),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get document with id %s: %v", id, err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read document data: %v", err)
	}

	document := core.Document{
		Data: *bytes.NewBuffer(data),
	}

	return &document, nil
}

func (s *s3Store) Create(ctx context.Context, document *core.Document) (string, error) {
	id := ulid.Make().String()

	_, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(id),
		Body:   bytes.NewReader(document.Data.Bytes()),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload document: %v", err)
	}

	return id, nil
}

// CanvasStore implementation for user-owned canvases
func (s *s3Store) getCanvasKey(userID, canvasID string) (string, error) {
	// Sanitize canvasID to prevent path traversal attacks.
	// It should be a simple name, not a path.
	if path.Base(canvasID) != canvasID {
		return "", fmt.Errorf("invalid canvas id: must not be a path")
	}
	if canvasID == "" || canvasID == "." || canvasID == ".." {
		return "", fmt.Errorf("invalid canvas id: must not be empty or a dot directory")
	}
	return path.Join(userID, canvasID), nil
}

func (s *s3Store) List(ctx context.Context, userID string) ([]*core.Canvas, error) {
	prefix := userID + "/"
	output, err := s.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list canvases for user %s: %v", userID, err)
	}

	canvases := make([]*core.Canvas, 0, len(output.Contents))
	for _, object := range output.Contents {
		resp, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    object.Key,
		})
		if err != nil {
			log.Printf("warn: failed to get object %s: %v", *object.Key, err)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("warn: failed to read object body %s: %v", *object.Key, err)
			continue
		}

		var canvas core.Canvas
		if err := json.Unmarshal(data, &canvas); err != nil {
			log.Printf("warn: failed to unmarshal canvas %s: %v", *object.Key, err)
			continue
		}

		// For list view, we don't need the full data blob.
		canvas.Data = nil
		canvases = append(canvases, &canvas)
	}

	return canvases, nil
}

// ListMeta lists a user's canvases from object metadata alone: one request per
// thousand objects instead of one per object.
func (s *s3Store) ListMeta(ctx context.Context, userID string) ([]core.CanvasMeta, error) {
	paginator := s3.NewListObjectsV2Paginator(s.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(userID + "/"),
	})

	var metas []core.CanvasMeta
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list canvases for user %s: %v", userID, err)
		}
		for _, object := range page.Contents {
			meta := core.CanvasMeta{ID: path.Base(aws.ToString(object.Key))}
			if object.LastModified != nil {
				meta.UpdatedAt = *object.LastModified
			}
			metas = append(metas, meta)
		}
	}
	return metas, nil
}

func (s *s3Store) Get(ctx context.Context, userID, id string) (*core.Canvas, error) {
	key, err := s.getCanvasKey(userID, id)
	if err != nil {
		return nil, err
	}
	resp, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// A specific check for NoSuchKey can be useful here.
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, fmt.Errorf("canvas not found")
		}
		return nil, fmt.Errorf("failed to get canvas %s: %v", id, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read canvas data: %v", err)
	}

	var canvas core.Canvas
	if err := json.Unmarshal(data, &canvas); err != nil {
		return nil, fmt.Errorf("failed to unmarshal canvas data: %v", err)
	}

	return &canvas, nil
}

func (s *s3Store) Save(ctx context.Context, canvas *core.Canvas) error {
	key, err := s.getCanvasKey(canvas.UserID, canvas.ID)
	if err != nil {
		return err
	}

	// Preserve CreatedAt on update
	if canvas.CreatedAt.IsZero() {
		existing, err := s.Get(ctx, canvas.UserID, canvas.ID)
		if err == nil && existing != nil {
			canvas.CreatedAt = existing.CreatedAt
		} else {
			canvas.CreatedAt = time.Now()
		}
	}
	canvas.UpdatedAt = time.Now()

	data, err := json.Marshal(canvas)
	if err != nil {
		return fmt.Errorf("failed to marshal canvas: %v", err)
	}

	_, err = s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to save canvas %s: %v", canvas.ID, err)
	}
	return nil
}

func (s *s3Store) Delete(ctx context.Context, userID, id string) error {
	key, err := s.getCanvasKey(userID, id)
	if err != nil {
		return err
	}
	_, err = s.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete canvas %s: %v", id, err)
	}
	return nil
}
