package api

import (
	"net/http"

	"github.com/cloudground-it/cloudground/internal/accounts"
)

// User management (SPEC.md §2.5): accounts.Service decides, these answer.

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.accounts.ListUsers()
	if err != nil {
		internalError(w, "ListUsers", err, "")
		return
	}
	respond(w, http.StatusOK, list)
}

// userBody is an account as the users page sends it.
type userBody struct {
	Email     string  `json:"email"`
	Password  string  `json:"password"`
	Role      string  `json:"role"`
	SiteIDs   []int64 `json:"site_ids"`
	FirstName string  `json:"first_name"`
	LastName  string  `json:"last_name"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, c *call) {
	var in userBody
	if !decodeOr400(w, r, &in) {
		return
	}
	u, err := s.accounts.CreateUser(r.Context(), c.Actor(), accounts.UserForm{
		Email: in.Email, Password: in.Password, Role: in.Role, SiteIDs: in.SiteIDs,
		FirstName: in.FirstName, LastName: in.LastName,
	})
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusCreated, u)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		FirstName string  `json:"first_name"`
		LastName  string  `json:"last_name"`
		Role      string  `json:"role"`
		SiteIDs   []int64 `json:"site_ids"`
		Disabled  bool    `json:"disabled"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	u, err := s.accounts.UpdateUser(c.User, c.Actor(), id, accounts.UserForm{
		Role: in.Role, SiteIDs: in.SiteIDs, FirstName: in.FirstName, LastName: in.LastName, Disabled: in.Disabled,
	})
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, u)
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	if err := s.accounts.ResetPassword(r.Context(), c.User, c.Actor(), id, in.Password); err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "password reset"})
}

func (s *Server) handleResetUserTwoFactor(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.accounts.ResetTwoFactor(c.User, c.Actor(), id); err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "two-factor reset"})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.accounts.DeleteUser(c.User, c.Actor(), id); err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "deleted"})
}
