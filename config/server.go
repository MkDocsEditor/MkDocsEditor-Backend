package config

type (
	ServerConfiguration struct {
		Host      string
		Port      int
		BasicAuth AuthenticationConfiguration
	}

	AuthenticationConfiguration struct {
		User     string
		Password string
	}
)
