package configurator

import "github.com/browserscale/browserscale-kit/form"

// Renderer is the UI layer the Configurator delegates to. The terminal uses
// HuhRenderer; browserscale Studio draws the same schema from its JSON Schema
// export and writes config.json itself.
type Renderer interface {
	// Greet is called once before the first prompt.
	Greet(title string)

	// EditConfig presents the form for editing. Each call mutates the
	// underlying typed cfg via the schema's bound pointers. The
	// implementation is responsible for calling schema.Validate against
	// filesDirectory after submission and, on failure, re-presenting the
	// form with errors visible. Returns nil only when the form is valid.
	EditConfig(schema *form.Schema, filesDirectory string) error

	// PromptRunName collects a name for this run's output folder under
	// runsDirectory. Existing names are allowed (resume / append).
	PromptRunName(runsDirectory string) (string, error)
}
