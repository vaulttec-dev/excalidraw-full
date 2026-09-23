// Package me tells the editor who the current session belongs to, so it can
// show the signed-in account next to the sign-out entry.
package me

import (
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
	"net/http"

	"github.com/go-chi/render"
)

type account struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
}

func HandleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := r.Context().Value(middleware.ClaimsContextKey).(*auth.AppClaims)
	if !ok || claims == nil {
		// Let through by the API token rather than a login: there is no account.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	render.JSON(w, r, account{Login: claims.Login, Name: claims.Name, AvatarURL: claims.AvatarURL})
}
