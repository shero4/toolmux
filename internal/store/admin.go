package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidLogin = errors.New("invalid username or password")

// User is a browser identity. Agent bearer tokens are independent identities.
type User struct {
	ID, Username, Role string
	Enabled            bool
	CreatedAt          time.Time
}

func (u User) CanManage() bool { return u.Enabled && (u.Role == "admin" || u.Role == "operator") }
func (u User) IsAdmin() bool   { return u.Enabled && u.Role == "admin" }

func (s *Store) AdminConfigured(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists)
	return exists, err
}
func PasswordHash(password string) ([]byte, error) {
	if len(password) < 12 || len(password) > 72 {
		return nil, errors.New("use a password between 12 and 72 bytes")
	}
	return bcrypt.GenerateFromPassword([]byte(password), 12)
}
func validUser(username, role string) bool {
	return strings.TrimSpace(username) != "" && len(username) <= 120 && (role == "admin" || role == "operator" || role == "viewer")
}

// CreateAdmin is first-run setup only. The shared lock serializes setup and
// role changes across all application instances.
func (s *Store) CreateAdmin(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	if !validUser(username, "admin") {
		return errors.New("invalid username")
	}
	hash, err := PasswordHash(password)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(722834682)`); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errors.New("an administrator is already configured")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO users(username,password_hash,role) VALUES($1,$2,'admin')`, username, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Compare a real hash even for unknown usernames to avoid a fast failure path.
var dummyPasswordHash, _ = bcrypt.GenerateFromPassword([]byte("unused-login-comparison"), 12)

func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (User, error) {
	var user User
	var hash []byte
	err := s.pool.QueryRow(ctx, `SELECT id,username,role,enabled,created_at,password_hash FROM users WHERE username=$1`, strings.TrimSpace(username)).Scan(&user.ID, &user.Username, &user.Role, &user.Enabled, &user.CreatedAt, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return User{}, err
	}
	if err != nil {
		hash = dummyPasswordHash
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || err != nil || !user.Enabled {
		return User{}, ErrInvalidLogin
	}
	return user, nil
}
func (s *Store) VerifyAdmin(ctx context.Context, username, password string) error {
	_, err := s.AuthenticateUser(ctx, username, password)
	return err
}
func (s *Store) NewAdminSession(ctx context.Context, userID string) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	result, err := s.pool.Exec(ctx, `WITH expired AS (DELETE FROM admin_sessions WHERE expires_at<=now()) INSERT INTO admin_sessions(token_hash,expires_at,user_id) SELECT $1,now()+interval '12 hours',id FROM users WHERE id=$2 AND enabled`, hash, userID)
	if err == nil && result.RowsAffected() != 1 {
		err = ErrInvalidLogin
	}
	return token, err
}
func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	hash := sha256.Sum256([]byte(token))
	var user User
	err := s.pool.QueryRow(ctx, `SELECT u.id,u.username,u.role,u.enabled,u.created_at FROM admin_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND u.enabled`, hash[:]).Scan(&user.ID, &user.Username, &user.Role, &user.Enabled, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrInvalidLogin
	}
	return user, err
}
func (s *Store) DeleteAdminSession(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash=$1`, hash[:])
	return err
}
func (s *Store) ChangeUserPassword(ctx context.Context, userID, current, next string) error {
	hash, err := PasswordHash(next)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var old []byte
	if err = tx.QueryRow(ctx, `SELECT password_hash FROM users WHERE id=$1 AND enabled FOR UPDATE`, userID).Scan(&old); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword(old, []byte(current)) != nil {
		return ErrInvalidLogin
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET password_hash=$2 WHERE id=$1`, userID, hash); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM admin_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,username,role,enabled,created_at FROM users ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.ID, &u.Username, &u.Role, &u.Enabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

var ErrUserAccess = errors.New("administrator access required")
var ErrLastAdmin = errors.New("keep at least one active administrator")

func authorizeUserChange(ctx context.Context, tx pgx.Tx, actorID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(722834682)`); err != nil {
		return err
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND enabled AND role='admin')`, actorID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrUserAccess
	}
	return nil
}
func (s *Store) CreateUser(ctx context.Context, actorID, username, password, role string) error {
	username = strings.TrimSpace(username)
	if !validUser(username, role) {
		return errors.New("invalid username or role")
	}
	hash, err := PasswordHash(password)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = authorizeUserChange(ctx, tx, actorID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO users(username,password_hash,role) VALUES($1,$2,$3)`, username, hash, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) SetUserAccess(ctx context.Context, actorID, userID, role string, enabled bool) error {
	if !validUser("user", role) {
		return errors.New("invalid role")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = authorizeUserChange(ctx, tx, actorID); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE users SET role=$2,enabled=$3 WHERE id::text=$1`, userID, role, enabled)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrNotFound
	}
	var admins int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE enabled AND role='admin'`).Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		return ErrLastAdmin
	}
	if _, err = tx.Exec(ctx, `DELETE FROM admin_sessions WHERE user_id::text=$1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// A shared database counter bounds password work across instances and restarts.
func (s *Store) AllowLogin(ctx context.Context, address string) (bool, error) {
	var count int
	err := s.pool.QueryRow(ctx, `INSERT INTO login_attempts(address,attempts,reset_at) VALUES($1,1,now()+interval '15 minutes')
        ON CONFLICT(address) DO UPDATE SET attempts=CASE WHEN login_attempts.reset_at<=now() THEN 1 ELSE login_attempts.attempts+1 END,
        reset_at=CASE WHEN login_attempts.reset_at<=now() THEN now()+interval '15 minutes' ELSE login_attempts.reset_at END
        RETURNING attempts`, address).Scan(&count)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM login_attempts WHERE reset_at<$1`, time.Now().Add(-time.Hour))
	return count <= 10, nil
}
