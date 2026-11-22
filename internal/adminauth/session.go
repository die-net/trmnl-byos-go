package adminauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/apitoken"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

const (
	// CookieName is the name of the session cookie.
	CookieName = "trmnl_session"
	// Lifetime is how long a session lasts after it was last used.
	Lifetime = 30 * 24 * time.Hour
	// touchEvery is the least time between updates of a session's
	// last_seen_at, so a busy page does not write on every request.
	touchEvery = time.Minute
)

// Login checks password and, if it is the admin password, opens a session and
// returns its token, which is shown only to the browser it is for. Only its
// hash is stored. Sessions that have expired are deleted. The errors are those
// of CheckPassword.
func Login(ctx context.Context, client *db.Client, password string, now time.Time) (string, error) {
	if err := CheckPassword(ctx, client, password); err != nil {
		return "", err
	}
	token, err := apitoken.New()
	if err != nil {
		return "", err
	}
	err = client.InTx(ctx, func(q *db.Queries) error {
		if err := q.DeleteExpiredAdminSessions(ctx, now.Unix()); err != nil {
			return fmt.Errorf("delete expired sessions: %w", err)
		}
		err := q.CreateAdminSession(ctx, db.CreateAdminSessionParams{
			TokenHash:  apitoken.Hash(token),
			CreatedAt:  now.Unix(),
			LastSeenAt: now.Unix(),
			ExpiresAt:  now.Add(Lifetime).Unix(),
		})
		if err != nil {
			return fmt.Errorf("save session: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Logout ends the session with the given token. A token that is not a
// session is not an error.
func Logout(ctx context.Context, client *db.Client, token string) error {
	if err := client.Write.DeleteAdminSessionByHash(ctx, apitoken.Hash(token)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// ValidSession reports whether token is a session that has not expired. A
// session last seen over a minute ago is touched, which moves its expiry
// out to a full lifetime from now.
func ValidSession(ctx context.Context, client *db.Client, token string, now time.Time) (bool, error) {
	if token == "" {
		return false, nil
	}
	s, err := client.Read.GetAdminSessionByHash(ctx, apitoken.Hash(token))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up session: %w", err)
	}
	if s.ExpiresAt <= now.Unix() {
		return false, nil
	}
	if now.Sub(time.Unix(s.LastSeenAt, 0)) >= touchEvery {
		err := client.Write.TouchAdminSession(ctx, db.TouchAdminSessionParams{
			LastSeenAt: now.Unix(), ExpiresAt: now.Add(Lifetime).Unix(), ID: s.ID,
		})
		if err != nil && !errkind.Ended(ctx, err) {
			slog.Warn("update session last seen", "err", err)
		}
	}
	return true, nil
}

// SignedIn reports whether the request carries the cookie of a valid
// session.
func SignedIn(ctx context.Context, client *db.Client, r *http.Request, now time.Time) (bool, error) {
	return ValidSession(ctx, client, Token(r), now)
}

// Token returns the session token in the request's cookie, or "".
func Token(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// NewCookie returns the cookie that holds a session token. It is Secure when
// secure is set, which is for a request that came over TLS or a server that
// is addressed by an https URL.
func NewCookie(token string, secure bool) *http.Cookie {
	return cookie(token, int(Lifetime/time.Second), secure)
}

// ClearCookie returns a cookie that makes the browser forget the session
// cookie.
func ClearCookie(secure bool) *http.Cookie {
	return cookie("", -1, secure)
}

// cookie builds the session cookie with the given value and Max-Age.
func cookie(value string, maxAge int, secure bool) *http.Cookie {
	//nolint:gosec // G124 wants Secure always set. A server reached over plain http on a LAN could then never sign in, so Secure follows the caller, which sets it whenever the browser used https.
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
}
