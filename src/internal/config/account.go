package config

// Account selects one bucket per identifier extracted from Authorization.
// JWTClaim names a top-level string claim; extraction does not verify signatures.
type Account struct {
	Enabled  bool   `yaml:"enabled"`
	JWTClaim string `yaml:"jwt_claim"`
}
