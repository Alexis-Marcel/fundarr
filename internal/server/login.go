package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"rsc.io/qr"
)

// Sign-ins of sources that need the user now and then (source.Interactive):
// start one, follow it, answer its code, drop it. The sync route starts one
// itself when the source has no session, so a single tap does it all.

// loginView is a sign-in as the UI shows it: a QR challenge comes drawn.
type loginView struct {
	*engine.Login
	QR string `json:"qr,omitempty"` // SVG of the QR code to scan
}

func viewLogin(l *engine.Login) *loginView {
	if l == nil {
		return nil
	}
	v := &loginView{Login: l}
	if l.Challenge != nil && l.Challenge.Kind == "qr" && l.Challenge.Data != "" {
		v.QR = qrSVG(l.Challenge.Data)
		l.Challenge.Data = ""
	}
	return v
}

// qrSVG draws a QR code as an SVG path, quiet zone included.
func qrSVG(text string) string {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return ""
	}
	var b strings.Builder
	n := c.Size + 8
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

func (s *Server) loginErr(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrNotInteractive) {
		writeErr(w, 400, err.Error())
		return
	}
	writeErr(w, 422, err.Error())
}

// POST /api/sources/{id}/login {method}
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	id := r.PathValue("id")
	if _, err := s.st.Source(r.Context(), id); err != nil {
		writeErr(w, 404, "no such source")
		return
	}
	l, err := s.eng.StartLogin(s.cfg.Background, id, in.Method)
	if err != nil {
		s.loginErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"login": viewLogin(l)})
}

// GET /api/sources/{id}/login
func (s *Server) loginState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"login": viewLogin(s.eng.LoginState(r.PathValue("id")))})
}

// POST /api/sources/{id}/login/code {code}
func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	l, err := s.eng.AnswerLogin(s.cfg.Background, r.PathValue("id"), in.Code)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]any{"login": viewLogin(l)})
}

// DELETE /api/sources/{id}/login
func (s *Server) cancelLogin(w http.ResponseWriter, r *http.Request) {
	s.eng.CancelLogin(r.PathValue("id"))
	writeJSON(w, map[string]bool{"ok": true})
}
