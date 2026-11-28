package desktopupdate

import "testing"

func windowsPolicyConfigFixture(t *testing.T) Config {
	t.Helper()
	config := newPolicyFixture(t).config
	config.TrustDomain = "fixture-windows-policy"
	config.Mode = "windows-policy"
	config.ReleaseID = "windows-amd64-1.0.0"
	config.Platform = "windows"
	config.Arch = "amd64"
	config.SigningIdentity = "FIXTURE_WINDOWS_RELEASE"
	config.UpdaterProtocol = 2
	config.PolicyURL = "https://updates.example.invalid/windows-policy/stable.json"
	return config
}

func TestWindowsPolicyConfigAcceptsStableWindowsOfficial(t *testing.T) {
	config := windowsPolicyConfigFixture(t)
	if UnsignedTestBuild {
		if err := ValidateConfig(config); ErrorCode(err) != "configuration_invalid" {
			t.Fatalf("isolated test build accepted the Windows official configuration: %v", err)
		}
		return
	}
	for _, version := range []string{"1.0.0", "1.0.0+windows-build.1"} {
		t.Run(version, func(t *testing.T) {
			config.Version = version
			if err := ValidateConfig(config); err != nil {
				t.Fatalf("stable Windows policy configuration rejected: %v", err)
			}
		})
	}
}

func TestWindowsPolicyConfigRejectsInvalidSettings(t *testing.T) {
	valid := windowsPolicyConfigFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"other platform with supported arch", func(c *Config) { c.Platform = "darwin" }},
		{"other arch", func(c *Config) { c.Arch = "arm64" }},
		{"personal audience", func(c *Config) { c.Audience = "personal-test" }},
		{"canary channel", func(c *Config) { c.Channel = "canary" }},
		{"beta channel", func(c *Config) { c.Channel = "beta" }},
		{"protocol one", func(c *Config) { c.UpdaterProtocol = 1 }},
		{"prerelease", func(c *Config) { c.Version = "1.0.0-rc.1" }},
		{"invalid release version", func(c *Config) { c.Version = "1.0" }},
		{"missing mode", func(c *Config) { c.Mode = "" }},
		{"unknown mode", func(c *Config) { c.Mode = "windows-policy-unknown" }},
		{"missing signing identity", func(c *Config) { c.SigningIdentity = "" }},
		{"missing public key", func(c *Config) { c.PublicKeys = nil }},
		{"malformed public key", func(c *Config) { c.PublicKeys = map[string]string{"key1": "not-base64"} }},
		{"public key with wrong length", func(c *Config) { c.PublicKeys = map[string]string{"key1": "YQ=="} }},
		{"missing allowed origin", func(c *Config) { c.AllowedOrigins = nil }},
		{"HTTP policy", func(c *Config) { c.PolicyURL = "http://updates.example.invalid/stable.json" }},
		{"unpinned policy origin", func(c *Config) { c.PolicyURL = "https://other.example.invalid/stable.json" }},
		{"invalid allowed origin", func(c *Config) {
			c.AllowedOrigins = []string{"https://updates.example.invalid", "http://other.example.invalid"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := valid
			tc.change(&config)
			if err := ValidateConfig(config); ErrorCode(err) != "configuration_invalid" {
				t.Fatalf("got %v, want configuration_invalid", err)
			}
		})
	}
}

func TestWindowsPolicyConfigPreservesOfficialModes(t *testing.T) {
	if UnsignedTestBuild {
		t.Skip("existing official configs belong to ordinary desktop builds")
	}
	mac := newPolicyFixture(t).config
	windows := windowsPolicyConfigFixture(t)
	windows.Mode = "official"
	for _, tc := range []struct {
		name   string
		config Config
	}{
		{"macOS official protocol one", mac},
		{"Windows official protocol two", windows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, channel := range []string{"stable", "beta", "canary"} {
				config := tc.config
				config.Channel = channel
				if err := ValidateConfig(config); err != nil {
					t.Errorf("existing %s channel rejected: %v", channel, err)
				}
			}
		})
	}
}
