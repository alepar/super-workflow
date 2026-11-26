package wf

// usesTracker interprets statically declared Beads operations as a tracker
// declaration. Cache the result for the sealed definition tree. This affects
// instructions only and does not change the graph or its hash.
func (d *Def) usesTracker() bool {
	if d == nil {
		return false
	}
	d.trackerOnce.Do(func() {
		d.hasTracker = viewUsesTracker(inspectStructure(d))
	})
	return d.hasTracker
}

func viewUsesTracker(view DefView) bool {
	for _, state := range view.States {
		for _, use := range state.EffectKinds() {
			if use.Adapter == "bd" {
				return true
			}
		}
		for _, use := range state.ObservationKinds() {
			if use.Adapter == "bd" {
				return true
			}
		}
	}
	for _, child := range view.Children {
		if viewUsesTracker(child) {
			return true
		}
	}
	return false
}
