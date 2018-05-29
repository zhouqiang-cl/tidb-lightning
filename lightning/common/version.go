package common

import (
	"fmt"

	log "github.com/sirupsen/logrus"
)

// Version information.
var (
	ReleaseVersion = "None"
	BuildTS        = "None"
	GitHash        = "None"
	GitBranch      = "None"
	GoVersion      = "None"
)

// GetRawInfo do what its name tells
func GetRawInfo() string {
	var info string
	info += fmt.Sprintf("Release Version: %s\n", ReleaseVersion)
	info += fmt.Sprintf("Git Commit Hash: %s\n", GitHash)
	info += fmt.Sprintf("Git Branch: %s\n", GitBranch)
	info += fmt.Sprintf("UTC Build Time: %s\n", BuildTS)
	info += fmt.Sprintf("Go Version: %s\n", GoVersion)
	return info
}

// PrintInfo prints some information of the app, like git hash, binary build time, etc.
func PrintInfo(app string, callback func()) {
	oriLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	printInfo(app)
	if callback != nil {
		callback()
	}
	log.SetLevel(oriLevel)
}

func printInfo(app string) {
	log.Infof("Welcome to %s", app)
	log.Infof("Release Version: %s", ReleaseVersion)
	log.Infof("Git Commit Hash: %s", GitHash)
	log.Infof("Git Branch: %s", GitBranch)
	log.Infof("UTC Build Time: %s", BuildTS)
	log.Infof("Go Version: %s", GoVersion)
}