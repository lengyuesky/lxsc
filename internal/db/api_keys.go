package db

import "context"

type APIKey struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  int64  `json:"expiresAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
}

func (d *DB) CreateExpiringAPIKey(ctx context.Context, userID int64, key, label string, expires int64) error {
	digest := keyDigest(key)
	_, err := d.sql.ExecContext(ctx, `INSERT INTO api_keys(key,id,user_id,label,created_at,expires_at) VALUES(?,?,?,?,?,?)`, digest, digest, userID, label, now(), expires)
	return err
}
func (d *DB) ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,label,created_at,expires_at,last_used_at FROM api_keys WHERE user_id=? ORDER BY created_at DESC,id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err = rows.Scan(&k.ID, &k.Label, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func (d *DB) RevokeAPIKey(ctx context.Context, userID int64, id string) error {
	result, err := d.sql.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id=? AND id=?`, userID, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
