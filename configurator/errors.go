package configurator

import "errors"

// ErrCanceled is returned by Configurator.Run (and the Renderer methods
// underneath) when the user cancels the configuration flow — typically
// by pressing Esc or Ctrl-C at any prompt.
//
// Callers should treat ErrCanceled as a benign exit signal: log a short
// message ("Canceled by user.") and shut down without persisting partial
// state or treating it as an application error.
//
// Use errors.Is to detect it; the Renderer translates back-end specific
// cancel errors (e.g. huh/v2.ErrUserAborted) to this sentinel so call
// sites don't need to depend on huh directly.
var ErrCanceled = errors.New("configuration canceled by user")
