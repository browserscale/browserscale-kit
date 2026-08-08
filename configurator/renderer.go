package configurator

import (
	"github.com/browserscale/browserscale-kit/form"
	"github.com/browserscale/browserscale-kit/module"
)

// Renderer is the UI layer the Configurator delegates to. Splitting this
// out of the Configurator lets us swap CLI back-ends (huh today, a
// bubbletea-native renderer or a JSON-driven GUI tomorrow) without
// changing how modules expose their schemas or how task setup works.
type Renderer interface {
	// SelectModule asks the user to choose one module from the list.
	// Returns nil if the user cancels.
	SelectModule(modules []module.Module) (module.Module, error)

	// EditConfig presents the form for editing. Each call mutates the
	// underlying typed cfg via the schema's bound pointers. The
	// implementation is responsible for calling schema.Validate against
	// filesDirectory after submission and, on failure, re-presenting the
	// form with errors visible. Returns nil only when the form is valid.
	EditConfig(schema *form.Schema, filesDirectory string) error

	// PromptProjectName collects a name for the new task. The Renderer is
	// expected to reject names that contain reserved characters or that
	// collide with an existing directory under tasksDirectory.
	PromptProjectName(tasksDirectory string) (string, error)
}
