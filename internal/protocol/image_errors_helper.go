package protocol

// ErrorfImage 构造一个「配置缺失」类的图片错误。
// 放在 protocol 包是为了让 api 层不必直接构造 ImageGenerationError。
func ErrorfImage(message string) error {
	return &ImageGenerationError{
		Message:    message,
		StatusCode: 502,
		ErrorType:  "server_error",
		Code:       "upstream_error",
	}
}
