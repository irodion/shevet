package tmuxctl

// Event is a notification from the tmux control-mode stream. The concrete
// types are the three signals the Server acts on — pane output, topology
// drift, and control-client death — plus a catch-all for everything else.
//
// The set is deliberately coarse: tmux distinguishes a dozen notifications
// (%window-add, %layout-change, ...) but the Server reconciles its Pane
// registry from an authoritative list-panes snapshot on any of them, so they
// all collapse into TopologyEvent. New tmux notifications degrade to
// UnknownEvent instead of breaking the stream.
type Event interface{ isEvent() }

// OutputEvent carries bytes an application wrote to its pane's terminal.
// It carries the payload of both %output and, once the pause-after flow
// control flag is set, %extended-output (ADR-0008): the two are the same
// signal, so the Server sees one event type either way.
type OutputEvent struct {
	// PaneID is the tmux pane id, e.g. "%3".
	PaneID string

	// Data is the raw output, decoded from tmux's escaping. Escape
	// sequences may be split across events.
	Data []byte

	// Seq is the event's position in the control stream, comparable with
	// the seq a CommandSeq reply reports: output with a smaller Seq was
	// already reflected in that command's view of the pane. Consumers use
	// this to order buffered output against capture-pane snapshots.
	Seq uint64
}

// PauseEvent signals that tmux has paused a pane's output (%pause), either
// because the Server asked (refresh-client -A '%id:pause') or because the
// pause-after backstop fired. No further output arrives for the pane until it
// is continued (refresh-client -A '%id:continue'), which re-seeds it.
type PauseEvent struct {
	// PaneID is the paused pane's tmux id.
	PaneID string
}

// ContinueEvent signals that a paused pane's output has resumed (%continue).
// It is informational: the Server drives the resume itself.
type ContinueEvent struct {
	// PaneID is the resumed pane's tmux id.
	PaneID string
}

// TopologyEvent signals that the set of panes — or their sizes or titles —
// may have changed and should be re-enumerated.
type TopologyEvent struct {
	// Notification is the raw notification line, for logging.
	Notification string
}

// ExitEvent signals that tmux is closing the control-mode client; no more
// events will follow.
type ExitEvent struct {
	// Reason is tmux's stated reason, often empty.
	Reason string
}

// UnknownEvent is a notification Shevet does not interpret.
type UnknownEvent struct {
	Line string
}

func (OutputEvent) isEvent()   {}
func (PauseEvent) isEvent()    {}
func (ContinueEvent) isEvent() {}
func (TopologyEvent) isEvent() {}
func (ExitEvent) isEvent()     {}
func (UnknownEvent) isEvent()  {}
