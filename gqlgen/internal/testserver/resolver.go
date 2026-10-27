package testserver

// THIS CODE WILL BE UPDATED WITH SCHEMA CHANGES. PREVIOUS IMPLEMENTATION FOR SCHEMA CHANGES WILL BE KEPT IN THE COMMENT SECTION. IMPLEMENTATION FOR UNCHANGED SCHEMA WILL BE KEPT.

import (
	"context"
)

type Resolver struct {
	Store *Store
}

// Owner is the resolver for the owner field.
func (r *itemResolver) Owner(ctx context.Context, obj *Item) (*Owner, error) {
	r.Store.mu.Lock()
	defer r.Store.mu.Unlock()
	return r.Store.copyOwner(obj.Owner.ID), nil
}

// Rename is the resolver for the rename field.
func (r *mutationResolver) Rename(ctx context.Context, id string, name string) (*Item, error) {
	r.Store.SetItemName(id, name)
	return r.Query().Item(ctx, id)
}

// Items is the resolver for the items field.
func (r *queryResolver) Items(ctx context.Context, first *int) ([]*Item, error) {
	r.Store.mu.Lock()
	defer r.Store.mu.Unlock()
	r.Store.count("items")
	var out []*Item
	for i, item := range r.Store.items {
		if first != nil && i >= *first {
			break
		}
		copied := r.Store.copyItem(item)
		if r.Store.Secret != "" {
			secret := r.Store.Secret
			copied.Secret = &secret
		}
		out = append(out, copied)
	}
	return out, nil
}

// Item is the resolver for the item field.
func (r *queryResolver) Item(ctx context.Context, id string) (*Item, error) {
	r.Store.mu.Lock()
	defer r.Store.mu.Unlock()
	r.Store.count("item")
	for _, item := range r.Store.items {
		if item.ID == id {
			copied := r.Store.copyItem(item)
			if r.Store.Secret != "" {
				secret := r.Store.Secret
				copied.Secret = &secret
			}
			return copied, nil
		}
	}
	return nil, nil
}

// Owner is the resolver for the owner field.
func (r *queryResolver) Owner(ctx context.Context, id string) (*Owner, error) {
	r.Store.mu.Lock()
	defer r.Store.mu.Unlock()
	r.Store.count("owner")
	return r.Store.copyOwner(id), nil
}

// Node is the resolver for the node field.
func (r *queryResolver) Node(ctx context.Context, id string) (Node, error) {
	r.Store.mu.Lock()
	r.Store.count("node")
	r.Store.mu.Unlock()
	if owner, _ := r.Owner(ctx, id); owner != nil {
		return owner, nil
	}
	if item, _ := r.Item(ctx, id); item != nil {
		return item, nil
	}
	return nil, nil
}

// Search is the resolver for the search field.
func (r *queryResolver) Search(ctx context.Context) ([]SearchResult, error) {
	items, _ := r.Items(ctx, nil)
	r.Store.mu.Lock()
	defer r.Store.mu.Unlock()
	var out []SearchResult
	for _, item := range items {
		out = append(out, item, r.Store.copyOwner(item.Owner.ID))
	}
	return out, nil
}

// Item returns ItemResolver implementation.
func (r *Resolver) Item() ItemResolver { return &itemResolver{r} }

// Mutation returns MutationResolver implementation.
func (r *Resolver) Mutation() MutationResolver { return &mutationResolver{r} }

// Query returns QueryResolver implementation.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

type itemResolver struct{ *Resolver }
type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
