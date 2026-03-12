package version

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
