package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	contract "github.com/quotacore/quotacore/api"
)

func (s *Service) listFeatures(w http.ResponseWriter, r *http.Request) {
	var kind string
	l, ok := s.startList(w, r, "features", func(q *query) []string {
		// The values are the FeatureKind enum's, which are the database's
		// (IP-17 DoD 6).
		kind = q.str("kind", nil, string(contract.FeatureKindMetered), string(contract.FeatureKindBoolean))
		return []string{kind}
	})
	if !ok {
		return
	}
	rows, err := s.store.ListFeatures(r.Context(), l.page, kind)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, cur, err := s.next(r, l, len(rows), func(i int) Position { return pos(rows[i].CreatedAt, rows[i].ID) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &contract.FeatureList{Features: rows[:n], NextCursor: cur})
}

// featureKey reads the path's feature key. One that cannot be a key names no
// feature, so it is a 404 rather than a 400, and it is never echoed (S1).
func featureKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := chi.URLParam(r, "feature_key")
	if !featureKeyShape.MatchString(key) {
		problem(w, r, notFound(contract.ErrorCodeFeatureNotFound))
		return "", false
	}
	return key, true
}

func (s *Service) getFeature(w http.ResponseWriter, r *http.Request) {
	key, valid := featureKey(w, r)
	if !valid {
		return
	}
	f, err := s.store.GetFeature(r.Context(), key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &f)
}

// createFeature creates a feature, metered unless kind says boolean. The kind
// is fixed here: no route changes it, so a boolean feature never has a
// metered assignment to conflict with (F-5, DR-050).
func (s *Service) createFeature(w http.ResponseWriter, r *http.Request) {
	b, err := readBody(r, "feature_key", "name", "unit", "description", "kind")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	key, _ := b.str("feature_key", true, featureKeyShape, 1, 64)
	name, _ := b.str("name", true, nil, 1, nameMax)
	unit, _ := b.str("unit", true, unitShape, 1, 32)
	desc, _ := b.str("description", false, nil, 0, textMax)
	kind, given := b.str("kind", false, nil, 0, 0)
	if given && kind != "metered" && kind != "boolean" {
		b.add("kind", "must be metered or boolean")
	}
	if p := b.check(&contract.FeatureCreate{}); p != nil {
		problem(w, r, p)
		return
	}
	if !given {
		kind = string(contract.FeatureKindMetered)
	}
	f, err := s.store.CreateFeature(r.Context(), actor(r), NewFeature{Key: key, Name: name, Unit: unit, Description: desc,
		Kind: kind})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, nil)
	respond(w, r, http.StatusCreated, &f)
}

// updateFeature renames or describes a feature, and changes its unit while
// nothing references it (INV-F2). A unit change on a referenced feature is
// 409 feature_unit_immutable and changes nothing.
func (s *Service) updateFeature(w http.ResponseWriter, r *http.Request) {
	key, valid := featureKey(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "version", "name", "description", "unit")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := FeatureChange{Version: b.version()}
	if v, set := b.str("name", false, nil, 1, nameMax); set {
		c.Name = &v
	}
	if v, set := b.str("description", false, nil, 0, textMax); set {
		c.Description = &v
	}
	if v, set := b.str("unit", false, unitShape, 1, 32); set {
		c.Unit = &v
	}
	if p := b.check(&contract.FeatureUpdate{}); p != nil {
		problem(w, r, p)
		return
	}
	f, err := s.store.UpdateFeature(r.Context(), actor(r), key, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, nil)
	respond(w, r, http.StatusOK, &f)
}

// archiveFeature stops new consumption and preserves every balance (DR-016,
// INV-F1). Every tenant whose plan holds the feature is re-versioned, so its
// snapshot answers feature_archived within the propagation bound.
func (s *Service) archiveFeature(w http.ResponseWriter, r *http.Request) {
	key, valid := featureKey(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "version", "reason")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	version, reason := b.version(), b.reason()
	if p := b.check(&contract.ArchiveRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	f, tenants, err := s.store.ArchiveFeature(r.Context(), actor(r), key, version, reason)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, tenants)
	respond(w, r, http.StatusOK, &f)
}

// deleteFeature hard-deletes a feature nothing references (F-6), and is 409
// feature_in_use otherwise.
func (s *Service) deleteFeature(w http.ResponseWriter, r *http.Request) {
	key, valid := featureKey(w, r)
	if !valid {
		return
	}
	b, err := readBody(r, "reason")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reason := b.reason()
	if p := b.check(&contract.DeleteRequest{}); p != nil {
		problem(w, r, p)
		return
	}
	if err := s.store.DeleteFeature(r.Context(), actor(r), key, reason); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publish(r, nil)
	noContent(w, r)
}
