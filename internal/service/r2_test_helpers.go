package service

// overrideEndpoint 把 R2 客户端指向测试服务器（仅测试用）。
func overrideEndpoint(client *R2Client, endpoint string) {
	client.overrideBase = endpoint
}
