// Package identity names a scimux data directory and the assets inside it.
//
// An installation ID is not a credential and must never be accepted as proof
// of identity. Anyone who can read the data directory can read the label, and
// a copied data directory is the same installation by definition.
package identity
