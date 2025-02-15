package namespace

import (
	"context"
	"fmt"

	"github.com/authzed/spicedb/pkg/graph"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/tuple"
)

// ReachabilityGraph is a helper struct that provides an easy way to determine all entrypoints
// for a subject of a particular type into a schema, for the purpose of walking from the subject
// to a specific resource relation.
type ReachabilityGraph struct {
	ts *TypeSystem
}

// ReachabilityEntrypoint is an entrypoint into the reachability graph for a subject of particular
// type.
type ReachabilityEntrypoint struct {
	re             *core.ReachabilityEntrypoint
	parentRelation *core.RelationReference
}

// EntrypointKind is the kind of the entrypoint.
func (re ReachabilityEntrypoint) EntrypointKind() core.ReachabilityEntrypoint_ReachabilityEntrypointKind {
	return re.re.Kind
}

// TupleToUserset returns the TTU associated with this entrypoint, if a TUPLESET_TO_USERSET_ENTRYPOINT.
func (re ReachabilityEntrypoint) TupleToUserset(nsDef *core.NamespaceDefinition) *core.TupleToUserset {
	if re.EntrypointKind() != core.ReachabilityEntrypoint_TUPLESET_TO_USERSET_ENTRYPOINT {
		panic(fmt.Sprintf("cannot call TupleToUserset for kind %v", re.EntrypointKind()))
	}

	if nsDef.Name != re.parentRelation.Namespace {
		panic("invalid namespace definition given to TupleToUserset")
	}

	for _, relation := range nsDef.Relation {
		if relation.Name == re.parentRelation.Relation {
			return graph.FindOperation[core.TupleToUserset](relation.GetUsersetRewrite(), re.re.OperationPath)
		}
	}

	return nil
}

// DirectRelation is the relation that this entrypoint represents, if a RELATION_ENTRYPOINT.
func (re ReachabilityEntrypoint) DirectRelation() *core.RelationReference {
	if re.EntrypointKind() != core.ReachabilityEntrypoint_RELATION_ENTRYPOINT {
		panic(fmt.Sprintf("cannot call DirectRelation for kind %v", re.EntrypointKind()))
	}

	return re.re.TargetRelation
}

// ContainingRelationOrPermission is the relation or permission containing this entrypoint.
func (re ReachabilityEntrypoint) ContainingRelationOrPermission() *core.RelationReference {
	return re.parentRelation
}

// IsDirectResult returns whether the entrypoint, when evaluated, becomes a direct result of
// the parent relation/permission. A direct result only exists if the entrypoint is not contained
// under an intersection or exclusion, which makes the entrypoint's object merely conditionally
// reachable.
func (re ReachabilityEntrypoint) IsDirectResult() bool {
	return re.re.ResultStatus == core.ReachabilityEntrypoint_DIRECT_OPERATION_RESULT
}

// ReachabilityGraphFor returns a reachability graph for the given namespace.
func ReachabilityGraphFor(ts *ValidatedNamespaceTypeSystem) *ReachabilityGraph {
	return &ReachabilityGraph{ts.TypeSystem}
}

// AllEntrypointsForSubjectToResource returns the entrypoints into the reachability graph, starting
// at the given subject type and walking to the given resource type.
func (rg *ReachabilityGraph) AllEntrypointsForSubjectToResource(
	ctx context.Context,
	subjectType *core.RelationReference,
	resourceType *core.RelationReference,
) ([]ReachabilityEntrypoint, error) {
	return rg.entrypointsForSubjectToResource(ctx, subjectType, resourceType, reachabilityFull)
}

// OptimizedEntrypointsForSubjectToResource returns the *optimized* set of entrypoints into the
// reachability graph, starting at the given subject type and walking to the given resource type.
//
// The optimized set will skip branches on intersections and exclusions in an attempt to minimize
// the number of entrypoints.
func (rg *ReachabilityGraph) OptimizedEntrypointsForSubjectToResource(
	ctx context.Context,
	subjectType *core.RelationReference,
	resourceType *core.RelationReference,
) ([]ReachabilityEntrypoint, error) {
	return rg.entrypointsForSubjectToResource(ctx, subjectType, resourceType, reachabilityOptimized)
}

func (rg *ReachabilityGraph) entrypointsForSubjectToResource(
	ctx context.Context,
	subjectType *core.RelationReference,
	resourceType *core.RelationReference,
	reachabilityOption reachabilityOption,
) ([]ReachabilityEntrypoint, error) {
	if resourceType.Namespace != rg.ts.nsDef.Name {
		return nil, fmt.Errorf("gave mismatching namespace name for resource type to reachability graph")
	}

	collected := &[]ReachabilityEntrypoint{}
	err := rg.collectEntrypoints(ctx, subjectType, resourceType, collected, map[string]struct{}{}, reachabilityOption)
	return *collected, err
}

func (rg *ReachabilityGraph) collectEntrypoints(
	ctx context.Context,
	subjectType *core.RelationReference,
	resourceType *core.RelationReference,
	collected *[]ReachabilityEntrypoint,
	encounteredRelations map[string]struct{},
	reachabilityOption reachabilityOption,
) error {
	// Ensure that we only process each relation once.
	key := relationKey(resourceType.Namespace, resourceType.Relation)
	if _, ok := encounteredRelations[key]; ok {
		return nil
	}

	encounteredRelations[key] = struct{}{}

	// Load the type system for the target resource relation.
	namespace, err := rg.ts.lookupNamespace(ctx, resourceType.Namespace)
	if err != nil {
		return err
	}

	rts, err := BuildNamespaceTypeSystem(namespace, rg.ts.lookupNamespace)
	if err != nil {
		return err
	}

	// TODO(jschorr): cache the graph somewhere.
	rrg := ReachabilityGraph{rts}

	relation, ok := rts.relationMap[resourceType.Relation]
	if !ok {
		return fmt.Errorf("unknown relation `%s` under namespace `%s` for reachability", resourceType.Relation, resourceType.Namespace)
	}

	// Decorate with operation paths, if necessary.
	derr := decorateRelationOpPaths(relation)
	if derr != nil {
		return derr
	}

	g, err := computeReachability(ctx, rrg.ts, resourceType.Relation, reachabilityOption)
	if err != nil {
		return err
	}

	// Add subject type entrypoints.
	subjectTypeEntrypoints, ok := g.EntrypointsBySubjectType[subjectType.Namespace]
	if ok {
		addEntrypoints(subjectTypeEntrypoints, resourceType, collected)
	}

	// Add subject relation entrypoints.
	subjectRelationEntrypoints, ok := g.EntrypointsBySubjectRelation[relationKey(subjectType.Namespace, subjectType.Relation)]
	if ok {
		addEntrypoints(subjectRelationEntrypoints, resourceType, collected)
	}

	// Recursively collect over any reachability graphs for subjects with non-ellipsis relations.
	for _, entrypointSet := range g.EntrypointsBySubjectRelation {
		if entrypointSet.SubjectRelation != nil && entrypointSet.SubjectRelation.Relation != tuple.Ellipsis {
			err := rrg.collectEntrypoints(ctx, subjectType, entrypointSet.SubjectRelation, collected, encounteredRelations, reachabilityOption)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func addEntrypoints(entrypoints *core.ReachabilityEntrypoints, parentRelation *core.RelationReference, collected *[]ReachabilityEntrypoint) {
	for _, entrypoint := range entrypoints.Entrypoints {
		*collected = append(*collected, ReachabilityEntrypoint{entrypoint, parentRelation})
	}
}
