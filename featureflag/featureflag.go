package featureflag

// FeatureFlag is a lookup map for features that is enabled or disabled
type FeatureFlag map[Flag]struct{}

// New return a new feature flags initialized with list of flags
func New(flags []string) FeatureFlag {
	featureFlag := make(FeatureFlag)
	for _, f := range flags {
		featureFlag[Flag(f)] = struct{}{}
	}
	return featureFlag
}

// IfSet runs function `do ` if flag is set in the feature flags
func (f FeatureFlag) IfSet(flag Flag, do func()) {
	if _, ok := f[flag]; !ok {
		return
	}
	do()
}

// IfNotSet runs function `do` if flag is not set in the feature flags
func (f FeatureFlag) IfNotSet(flag Flag, do func()) {
	if _, ok := f[flag]; ok {
		return
	}
	do()
}
