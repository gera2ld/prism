package gateway

import "regexp"

// Key policy: optional RE2 whitelist patterns. A nil pattern allows all.
// All three dimensions must pass for a route target to be usable.

// Allows reports whether the key may use alias via target.
func (k Key) Allows(alias string, t Target) bool {
	if k.AliasPattern != nil && !k.AliasPattern.MatchString(alias) {
		return false
	}
	if k.ProviderPattern != nil && !k.ProviderPattern.MatchString(t.ProviderName) {
		return false
	}
	if k.ModelPattern != nil && !k.ModelPattern.MatchString(t.Model) {
		return false
	}
	return true
}

// Authorize returns the first priority-ordered target the key may use.
func Authorize(key Key, alias string, targets []Target) (Target, error) {
	for _, t := range targets {
		if key.Allows(alias, t) {
			return t, nil
		}
	}
	return Target{}, ErrForbidden
}

// CompilePolicy compiles non-empty whitelist patterns. Empty means allow all.
func CompilePolicy(aliasPattern, providerPattern, modelPattern string) (alias, provider, model *regexp.Regexp, err error) {
	compile := func(expr string) (*regexp.Regexp, error) {
		if expr == "" {
			return nil, nil
		}
		return regexp.Compile(expr)
	}
	if alias, err = compile(aliasPattern); err != nil {
		return nil, nil, nil, err
	}
	if provider, err = compile(providerPattern); err != nil {
		return nil, nil, nil, err
	}
	if model, err = compile(modelPattern); err != nil {
		return nil, nil, nil, err
	}
	return alias, provider, model, nil
}
