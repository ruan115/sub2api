package app

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/portunex/identity"
)

func (d *Demo) login(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required", "登录请求需要JSON")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 4096)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decoder.Decode(&input); err != nil {
		inputError(w, err)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		inputError(w, err)
		return
	}
	if len(input.Email) == 0 || len(input.Email) > 254 || len(input.Password) == 0 || len(input.Password) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_request", "登录字段无效")
		return
	}
	token, session, err := d.identity.Replace(input.Email, input.Password, cookieToken(r))
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrCredentials):
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "演示账户或密码错误")
		case errors.Is(err, identity.ErrCapacity):
			writeError(w, http.StatusTooManyRequests, "demo_capacity", "演示会话已达上限")
		default:
			writeError(w, http.StatusServiceUnavailable, "demo_unavailable", "本地演示暂不可用")
		}
		return
	}
	// A successful re-login retires the presented session. Never store raw tokens
	// in response JSON, logs or localStorage; Secure=false is local HTTP only.
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: token, Path: Prefix, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(identity.SessionTTL.Seconds()), Expires: session.ExpiresAt})
	writeJSON(w, http.StatusOK, session)
}

func (d *Demo) logout(w http.ResponseWriter, r *http.Request) {
	d.identity.Logout(cookieToken(r))
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: Prefix, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func inputError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "请求体过大")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "JSON请求无效")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{code, message},
	})
}
