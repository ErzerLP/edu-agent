//go:build unix

package offline

// Platform lease primitives live in internal/filelock. This file remains so
// platform-specific package layouts and downstream build filters stay stable.
