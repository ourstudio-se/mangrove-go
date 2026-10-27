package mangrove

import "fmt"

// requiredEntities holds entity ids by type, in insertion order.
type requiredEntities struct {
	typenames []string
	ids       map[string][]string
	seen      map[string]map[string]bool
}

func (r *requiredEntities) add(typename, id string) {
	if r.ids == nil {
		r.ids = map[string][]string{}
		r.seen = map[string]map[string]bool{}
	}
	seen := r.seen[typename]
	if seen == nil {
		seen = map[string]bool{}
		r.seen[typename] = seen
		r.typenames = append(r.typenames, typename)
	}
	if !seen[id] {
		seen[id] = true
		r.ids[typename] = append(r.ids[typename], id)
	}
}

type entityTreeNode struct {
	children      map[string]*entityTreeNode
	childrenOrder []string
	coordinates   string
	// isDirty: invalidated, or below an invalidated node within the same
	// segment: select in full.
	isDirty       bool
	isEntity      bool
	isInvalidated bool
	// hasOpaqueObjects: objects that aren't identifiable entities were seen
	// at this node.
	hasOpaqueObjects bool
	isList           bool
	// isRequired: on the path from the root to an invalidated entity.
	isRequired bool
	// linkable: every entity type seen at this node has a cache resolver, so
	// the node can be severed from the document and reached through a link
	// instead.
	linkable bool
	// requiredEntities must be re-resolved through this node's cache
	// resolvers.
	requiredEntities requiredEntities
	resolvers        CacheResolverMap
	typenames        map[string]bool
	typenameOrder    []string
}

func createNode(coordinates string) *entityTreeNode {
	return &entityTreeNode{
		children:    map[string]*entityTreeNode{},
		coordinates: coordinates,
		resolvers:   CacheResolverMap{},
		typenames:   map[string]bool{},
	}
}

func (n *entityTreeNode) child(field string) *entityTreeNode {
	return n.children[field]
}

type entityTree struct {
	// nodes holds every node, by coordinates.
	nodes map[string]*entityTreeNode
	// resolverTypenames are the types that have a cache resolver.
	resolverTypenames []string
	root              *entityTreeNode
}

// buildEntityTree builds the entity tree for a cached operation.
//
// The tree is built in two passes so that the outcome doesn't depend on the
// order of the entities: first every entity is placed, then invalidation
// state is propagated (finalizeEntityTree).
func buildEntityTree(entities []entityCacheResult, resolvers CacheResolverMap, opaqueCoordinates []string) (*entityTree, error) {
	root := createNode(rootQuery)
	nodes := map[string]*entityTreeNode{root.coordinates: root}
	var invalidatedPaths [][]*entityTreeNode
	var invalidatedEntities []entityCacheResult

	for _, ecr := range entities {
		if len(ecr.Path) == 0 || ecr.Path[0].Field != rootQuery {
			first := ""
			if len(ecr.Path) > 0 {
				first = ecr.Path[0].Field
			}
			return nil, fmt.Errorf("entity paths must start at %s, got %s", rootQuery, first)
		}

		node := root
		nodePath := []*entityTreeNode{root}

		for _, part := range ecr.Path[1:] {
			child := node.children[part.Field]
			if child == nil {
				child = createNode(node.coordinates + "." + part.Field)
				node.children[part.Field] = child
				node.childrenOrder = append(node.childrenOrder, part.Field)
				nodes[child.coordinates] = child
			}
			if part.Index != nil {
				child.isList = true
			}
			node = child
			nodePath = append(nodePath, node)
		}

		node.isEntity = true
		if !node.typenames[ecr.Entity.Typename] {
			node.typenames[ecr.Entity.Typename] = true
			node.typenameOrder = append(node.typenameOrder, ecr.Entity.Typename)
		}

		if ecr.invalidated {
			node.isInvalidated = true
			invalidatedPaths = append(invalidatedPaths, nodePath)
			invalidatedEntities = append(invalidatedEntities, ecr)
		}
	}

	for _, coordinates := range opaqueCoordinates {
		if node := nodes[coordinates]; node != nil {
			node.hasOpaqueObjects = true
		}
	}

	finalizeEntityTree(root, resolvers, invalidatedPaths, invalidatedEntities)

	resolverTypenames := make([]string, 0, len(resolvers))
	for typename := range resolvers {
		resolverTypenames = append(resolverTypenames, typename)
	}

	return &entityTree{nodes: nodes, resolverTypenames: resolverTypenames, root: root}, nil
}

func resetComputedState(node *entityTreeNode) {
	node.isDirty = false
	node.isRequired = false
	node.requiredEntities = requiredEntities{}
	for _, field := range node.childrenOrder {
		resetComputedState(node.children[field])
	}
}

func computeLinkability(node *entityTreeNode, resolvers CacheResolverMap) {
	node.resolvers = CacheResolverMap{}
	node.linkable = false

	if node.isEntity && !node.hasOpaqueObjects && len(node.typenameOrder) > 0 {
		all := true
		for _, typename := range node.typenameOrder {
			resolver, ok := resolvers[typename]
			if !ok {
				all = false
				break
			}
			node.resolvers[typename] = resolver
		}
		node.linkable = all
		if !all {
			node.resolvers = CacheResolverMap{}
		}
	}

	for _, field := range node.childrenOrder {
		computeLinkability(node.children[field], resolvers)
	}
}

// computeRequired marks the nodes leading to each invalidated entity as
// required, and decides at which node the invalidated data is re-resolved:
//
//   - at the invalidated entity itself, if it can be reached through a cache
//     resolver,
//   - otherwise at the closest ancestor entity that can, re-resolving that
//     ancestor (by its own id) with a selection narrowed down to the path,
//   - otherwise from the root of the operation.
//
// Returns false if a linkable node turned out to be unusable, in which case
// it has been made unlinkable and the pass must be repeated.
func computeRequired(invalidatedPaths [][]*entityTreeNode, invalidatedEntities []entityCacheResult) bool {
	for p, nodePath := range invalidatedPaths {
		ecr := invalidatedEntities[p]

		for i := len(nodePath) - 1; i >= 0; i-- {
			node := nodePath[i]
			node.isRequired = true

			if !node.linkable {
				continue
			}

			part := ecr.Path[i]
			isLeaf := i == len(nodePath)-1
			var typename string
			var id *string
			if isLeaf {
				typename = ecr.Entity.Typename
				id = ecr.Entity.ID
			} else {
				typename = part.Typename
				id = part.ID
			}

			if _, ok := node.resolvers[typename]; typename == "" || id == nil || !ok {
				// We know we need to pass through this node but can't tell
				// which entity to resolve; reach it from its parent instead.
				node.linkable = false
				node.resolvers = CacheResolverMap{}
				return false
			}

			node.requiredEntities.add(typename, *id)
			break
		}
	}

	return true
}

func computeDirty(node *entityTreeNode, parentDirty bool) {
	node.isDirty = node.isInvalidated || (parentDirty && !node.linkable)

	for _, field := range node.childrenOrder {
		computeDirty(node.children[field], node.isDirty)
	}
}

func finalizeEntityTree(root *entityTreeNode, resolvers CacheResolverMap, invalidatedPaths [][]*entityTreeNode, invalidatedEntities []entityCacheResult) {
	computeLinkability(root, resolvers)

	// Each failed round removes one linkable node, so this terminates.
	for {
		resetComputedState(root)
		if computeRequired(invalidatedPaths, invalidatedEntities) {
			break
		}
	}

	computeDirty(root, false)
}

// getSelectionsToKeep returns which response keys of the selection set at
// node to keep, or nil to keep the selection set in full.
func getSelectionsToKeep(node *entityTreeNode) []string {
	// Positions without entities are kept as far as their parent keeps them.
	if node == nil {
		return nil
	}

	// Invalidated nodes, and nodes below them in the same segment, are
	// fetched in full.
	if node.isDirty {
		return nil
	}

	if node.isRequired {
		// Traversed lists are always fetched in full, as their membership is
		// state-volatile.
		if node.isList {
			return nil
		}

		// Traversed objects that are not entities have no way of being
		// invalidated themselves, so they are fetched in full.
		if !node.isEntity {
			return nil
		}

		// Traversed entities only include the paths leading towards an
		// invalidated entity; if anything else had changed, they would have
		// been invalidated themselves. Their identity is kept so the merge
		// can verify it is still the same entity.
		keep := []string{AliasEntityCacheID, AliasEntityCacheTypename}
		for _, field := range node.childrenOrder {
			child := node.children[field]
			if child.isRequired || child.isDirty {
				keep = append(keep, field)
			}
		}
		return keep
	}

	// Linkable nodes keep their selections so they can be turned into links
	// (whose selection sets are used for follow-up link queries).
	if node.linkable {
		return nil
	}

	return []string{}
}
