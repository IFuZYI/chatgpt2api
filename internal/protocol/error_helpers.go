package protocol

import "errors"

// asError 是 errors.As 的薄包装，避免在多处重复 import errors。
func asError(err error, target any) bool { return errors.As(err, target) }
