//go:build !linux

package snapshots

import (
	"context"
	"fmt"
	"time"
)

// Восстановление опирается на openat(O_NOFOLLOW) Linux; на других ОС отказ.
func restoreCopy(context.Context, string, string, string, time.Time) (RestoreResult, error) {
	return RestoreResult{}, fmt.Errorf("%w: восстановление поддерживается только в Linux", ErrRefused)
}
