package tools

import "os"

func getEnv() []string { return os.Environ() }
