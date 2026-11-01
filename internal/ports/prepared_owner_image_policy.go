package ports

import "context"

// PreparedOwnerImagePolicy retains the original component's two references and
// source inputs without resolving either image. Runtime and renderer authority
// remain in the owning instance capsule. This is distinct from a concrete
// PreparedOwnerImageSource, whose version-1 meaning is unchanged.
type PreparedOwnerImagePolicy struct {
	Version           int
	Primary, Fallback []byte
	Kind              string
	Environment       string
	Project           []byte
	Guards            []OwnerImageSourceGuard
	Directory         []byte
	TokenPaths        [][]byte
	CacheBinding      string
	Cache             []OwnerImageCacheSuccessor
}

// OwnerImageCacheSuccessor contains only original guard transitions, never tokens.
type OwnerImageCacheSuccessor struct {
	Policy        string
	Before, After []OwnerImageSourceGuard
}
type NativeOwnerImageCacheObserver interface {
	RecordOwnerImageCache(OwnerImageCacheSuccessor) error
	ReadOwnerImageCache(string) ([]OwnerImageCacheSuccessor, error)
}
type nativeOwnerImageCacheKey struct{}

func WithNativeOwnerImageCache(ctx context.Context, observer NativeOwnerImageCacheObserver) context.Context {
	if observer == nil {
		return context.WithValue(ctx, nativeOwnerImageCacheKey{}, false)
	}
	return context.WithValue(ctx, nativeOwnerImageCacheKey{}, observer)
}
func NativeOwnerImageCacheFrom(ctx context.Context) NativeOwnerImageCacheObserver {
	v, _ := ctx.Value(nativeOwnerImageCacheKey{}).(NativeOwnerImageCacheObserver)
	return v
}
