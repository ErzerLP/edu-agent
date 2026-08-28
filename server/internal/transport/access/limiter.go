package access

type Limiter interface {
	Allow(string) bool
	Limited(string) bool
}
