package wf

import "fmt"

type artifactData struct {
	name, path, typ string
	producer        *node
	exported        bool
	via             *node
	repo            bool // a file the product repository keeps (NewRepoArtifact)
	optional        bool // an allocated artifact the producer may leave unwritten (Optional)
}

// Artifact is a typed artifact declaration: a content type, the producing
// state and its path. The engine allocates the path from the producing
// instance's identity (artifact-identity A1): two instances can never meet on
// one path, and a definition never composes it. A path template (the legacy
// three-argument form) still works until every client is migrated; the
// staged lint notes each one.
//
// Owns: the producer/consumer link the lint rules need (a gate presents only
// artifacts produced upstream; consumer type matches producer). Artifacts pass
// by reference (path plus hash), never inline.
// Does not: hold content; it is a reference.
// Extend by: a new artifact is a declaration on the producing state.
type Artifact[T any] struct{ a *artifactData }

// AnyArtifact is an Artifact of any content type.
type AnyArtifact interface{ artifact() *artifactData }

func (a Artifact[T]) artifact() *artifactData { return a.a }

func (a Artifact[T]) inputRef() inputDesc {
	return inputDesc{kind: "artifact", name: a.a.name, art: a.a}
}

// Name returns the artifact name.
func (a Artifact[T]) Name() string { return a.a.name }

// Path returns the path template, "" for an allocated artifact.
func (a Artifact[T]) Path() string { return a.a.path }

// NewArtifact declares an artifact on its producing state. Without a path
// template the engine allocates its path, <records>/artifacts/<producing
// instance>/<name>; name is then a plain file name. A pathTemplate (legacy)
// binds the path from params instead.
func NewArtifact[T any](producer State, name string, pathTemplate ...string) Artifact[T] {
	n := producer.node()
	path := ""
	if len(pathTemplate) > 0 {
		path = pathTemplate[0]
	}
	ad := &artifactData{name: name, path: path, typ: fmt.Sprintf("%T", *new(T)), producer: n}
	n.def.artifacts = append(n.def.artifacts, ad)
	return Artifact[T]{a: ad}
}

// Optional marks an allocated artifact its producer may leave unwritten (a
// roast with no findings, a step whose outcome has nothing to record): done is
// not refused while it is missing. It returns a.
func (a Artifact[T]) Optional() Artifact[T] {
	a.a.optional = true
	return a
}

// NewRepoArtifact declares an artifact that is a file of the product
// repository (a spec or a plan the run commits): its path is the product's,
// given by pathTemplate, never allocated under the run's records, and the
// staged lint leaves it alone.
func NewRepoArtifact[T any](producer State, name, pathTemplate string) Artifact[T] {
	a := NewArtifact[T](producer, name, pathTemplate)
	a.a.repo = true
	return a
}

// artifactToken prefixes the placeholder ArtifactPath returns.
const artifactToken = "artifact."

// ArtifactPath is the placeholder of a's rendered path, for a wf.Bound prompt
// input, a command's argv or an effect's arguments (a {} template): the engine binds it to the
// artifact's path in the instance that binds it (the allocated path, or the
// recorded one once the producer submitted). Consumers bind it; they never
// re-derive the path.
func ArtifactPath(a AnyArtifact) string { return "{" + artifactToken + a.artifact().name + "}" }

// Consumes declares what a state reads; lint checks each was produced upstream.
func Consumes(s State, as ...AnyArtifact) {
	n := s.node()
	for _, a := range as {
		n.consumes = append(n.consumes, a.artifact())
	}
}

// Export exposes a child artifact through the child definition's result.
func Export[T any](d *Def, a Artifact[T]) {
	a.a.exported = true
	d.exports = append(d.exports, a.a)
}

// FromChild is the parent-side view of an exported child artifact; lint reports
// RuleChildNotExported when the child does not export it.
func FromChild[T any](via Composite, a Artifact[T]) Artifact[T] {
	return Artifact[T]{a: &artifactData{name: a.a.name, path: a.a.path, typ: a.a.typ, producer: a.a.producer, exported: a.a.exported, via: via.node()}}
}

// Ref is a resolved artifact reference value (path plus hash): the form
// artifacts take inside Sub and Map I and R structs.
type Ref[T any] struct {
	// Path is validated (reference workflow S3a): a path, never an option,
	// so a placeholder bound to it may stand as a shell argument.
	Path string `pattern:"^[^-\\x00\\n][^\\x00\\n]*$"`
	Hash string
}

// artifactReferenceValue identifies the concrete generic type without changing its JSON encoding.
func (r Ref[T]) artifactReferenceValue() any { return r }

// RefOf resolves an artifact reference.
func RefOf[T any](c Ctx, a Artifact[T]) Ref[T] { return read[Ref[T]](c, "ref", a.a) }

// Read loads the artifact content, typed.
func Read[T any](c Ctx, a Artifact[T]) (T, error) {
	v, err := c.value(ctxKey{kind: "artifact", ref: a.a})
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeValue[T](v), nil
}
