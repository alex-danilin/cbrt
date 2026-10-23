// Package privacy holds the repository privacy audit (privacy_test.go): it
// fails if anything under testdata/ looks like it identifies a person, machine
// or account. All fixtures are synthetic; this guards against mistakes.
//
//	go test ./scripts/privacy
package privacy
