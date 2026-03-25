package framework

import "os"

const PostCleanUpEnv = "POST_CLEANUP"

func IsCleanUpNeeded() bool {
	return os.Getenv(PostCleanUpEnv) != "no"
}
