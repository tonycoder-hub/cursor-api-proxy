package cursor

// detectEnv supplies a synthetic context. The proxy never inspects the host
// workspace, shell, kernel version or files to satisfy upstream requests.
func detectEnv() *EnvContext {
	return &EnvContext{OSVersion: "linux", WorkspacePath: "/workspace", Shell: "sh", TimeZone: "UTC"}
}
