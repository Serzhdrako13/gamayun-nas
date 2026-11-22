//go:build !linux

package apps

type statT struct{ Mode uint32 }

type fdWalker struct {
	visit   func(path string, st *statT) error
	Skipped int64
	stop    func() bool
}

func (w *fdWalker) walk(string) error                        { return errOS }
func (c aclChange) apply(string, *statT) (bool, error)       { return false, errOS }
func readACLs(string) (ACL, ACL, error)                      { return nil, nil, errOS }
func chownTree(string, int, int, func() bool) (int64, error) { return 0, errOS }
