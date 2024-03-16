package configuration

type (
	ServerConfiguration struct {
		Host      string                      `yaml:"host"`
		Port      int                         `yaml:"port"`
		BasicAuth AuthenticationConfiguration `yaml:"basicAuth"`
		CORS      CorsConfiguration           `yaml:"cors"`
	}

	AuthenticationConfiguration struct {
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	}

	CorsConfiguration struct {
		AllowedOrigins []string `yaml:"allowedOrigins"`
		AllowedMethods []string `yaml:"allowedMethods"`
	}
)
