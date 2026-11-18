package clientui

// State describes the lifecycle state of the remote client window.
type State int

const (
	StateConnecting State = iota
	StateOnline
	StatePrompt
	StateActive
	StateError
)

// Callbacks holds user action callbacks invoked from the client GUI.
type Callbacks struct {
	OnAllow      func()
	OnDeny       func()
	OnDisconnect func()
	OnClose      func()
}

// MonitorBounds describes a physical display rectangle in virtual desktop coordinates.
type MonitorBounds struct {
	Index   int
	X       int
	Y       int
	Width   int
	Height  int
	Primary bool
}
