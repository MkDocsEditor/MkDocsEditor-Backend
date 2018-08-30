package config

type (
	ServerConfiguration struct {
		Host      string
		Port      int
		BasicAuth AuthenticationConfiguration
		CORS      CorsConfiguration
	}

	AuthenticationConfiguration struct {
		User     string
		Password string
	}

	CorsConfiguration struct {
		AllowedOrigins []string
		AllowedMethods []string
	}
)
