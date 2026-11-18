package version

import "fmt"

// Set at build time via -ldflags "-X remote-control/internal/version.Version=..."
var (
	Version = "1.0.0-dev"
	Commit  = ""
	Date    = ""
)

// String returns a human-readable version banner.
func String() string {
	s := Version
	if Commit != "" {
		s += " (" + Commit + ")"
	}
	if Date != "" {
		s += " " + Date
	}
	return s
}

// Banner formats a component name and its build version.
func Banner(component string) string {
	return fmt.Sprintf("Gecko %s %s", component, String())
}
