package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUpdateUserPasswordPreservesProfileAndRejectsStaleWrites(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "password.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	u, err := d.CreateUser(ctx, "alice", "original-encrypted", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateUser(ctx, u.ID, "renamed", "", true, "flac"); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			err := d.UpdateUserPassword(ctx, u.ID, u.PasswordEnc, "new-encrypted")
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrUserPasswordChanged) {
				t.Errorf("更新失败: %v", err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("同一旧密码只能成功修改一次，实际 %d", successes.Load())
	}
	after, err := d.GetUserByID(ctx, u.ID)
	if err != nil || after.Name != "renamed" || !after.IsAdmin || after.Quality != "flac" || after.PasswordEnc != "new-encrypted" || after.CreatedAt != u.CreatedAt {
		t.Fatalf("改密不得覆盖其他属性: %+v %v", after, err)
	}
	if err := d.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateUserPassword(ctx, u.ID, "new-encrypted", "another"); !errors.Is(err, ErrUserPasswordChanged) {
		t.Fatalf("已删除用户不得返回改密成功: %v", err)
	}
}
