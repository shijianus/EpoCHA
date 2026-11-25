package generator

import "errors"

var ErrProductionProfile = errors.New("generator: production profile 没有可用的生产就绪题型或包含未就绪题型")

// Lifecycle is shared by admission and the admin DTO. Unknown IDs fail closed;
// none of the current 19 generators has production approval (ADR-036).
func Lifecycle(id string) (experimental, productionReady bool) {
	switch id {
	case ArithID, TextImageID, TextImageV2ID, TextImageV3ID,
		ShapeGridID, ShapeCountID, ParallaxID, BankShotID,
		AccessibleChoiceID, EnergyWindowID, PipeConnectID, IntersectionPointID,
		TileRotationID, SymbolPathID, CrowdFindID, VisualRankID,
		ProgressiveTargetID, ShellTrackID, SilhouetteAssemblyID:
		return true, false
	default:
		return true, false
	}
}

func productionEligible(lifecycle func(string) (bool, bool), id string) bool {
	experimental, ready := lifecycle(id)
	return !experimental && ready
}

// RequireProduction permanently restricts this registry, including future
// registrations. Failure still leaves the restriction in place.
func (r *Registry) RequireProduction() error {
	return r.requireProduction(Lifecycle)
}

func (r *Registry) requireProduction(lifecycle func(string) (bool, bool)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lifecycle == nil {
		r.lifecycle = lifecycle
	}
	for id := range r.byID {
		if !productionEligible(r.lifecycle, id) {
			delete(r.byID, id)
		}
	}
	if len(r.byID) == 0 {
		return ErrProductionProfile
	}
	return nil
}
