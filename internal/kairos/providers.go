// Package kairos is the top-level options-research tool. This file's
// only purpose is to anonymously import every provider subpackage so
// their init()s run, registering each constructor with the factory in
// internal/kairos/provider.
//
// Why this lives in `kairos` (not `provider`): the imports form a
// natural one-way dependency graph — kairos depends on provider, each
// concrete provider depends on provider, nothing depends on kairos.
// Putting these imports inside `provider` would create a cycle since
// provider/kite imports provider to call Register().
//
// Adding a new provider:
//   1. Create internal/kairos/provider/<name>/
//   2. Implement BrokerProvider with an init() that registers itself
//   3. Add one anonymous import line below
package kairos

import (
	_ "github.com/TheBharathProject/sypher-api/internal/kairos/provider/angel"
	_ "github.com/TheBharathProject/sypher-api/internal/kairos/provider/dhan"
	_ "github.com/TheBharathProject/sypher-api/internal/kairos/provider/kite"
	_ "github.com/TheBharathProject/sypher-api/internal/kairos/provider/null"
	_ "github.com/TheBharathProject/sypher-api/internal/kairos/provider/upstox"
)
