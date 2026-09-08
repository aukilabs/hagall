package authpkg

import (
	"time"

	"github.com/golang-jwt/jwt/v4"
)

const SkewLeeway = 60 * time.Second

func parseTokenWithClaims(tokenStr string, claims jwt.Claims, keyFunc jwt.Keyfunc) (*jwt.Token, error) {
	parser := jwt.Parser{SkipClaimsValidation: true}
	token, err := parser.ParseWithClaims(tokenStr, claims, keyFunc)
	if err != nil {
		return token, err
	}
	if err := validateClaimsWithLeeway(claims); err != nil {
		return token, err
	}
	return token, nil
}

func validateClaimsWithLeeway(claims jwt.Claims) error {
	if claims == nil {
		return nil
	}
	err := claims.Valid()
	if err == nil {
		return nil
	}
	ve, ok := err.(*jwt.ValidationError)
	if !ok {
		return err
	}

	now := time.Now().UTC()
	nbf, iat, exp := extractClaimTimes(claims)
	errors := ve.Errors

	if errors&jwt.ValidationErrorNotValidYet != 0 && nbf != nil {
		if nbf.After(now) && nbf.Sub(now) <= SkewLeeway {
			errors &^= jwt.ValidationErrorNotValidYet
		}
	}

	if errors&jwt.ValidationErrorIssuedAt != 0 && iat != nil {
		if iat.After(now) && iat.Sub(now) <= SkewLeeway {
			errors &^= jwt.ValidationErrorIssuedAt
		}
	}

	if errors&jwt.ValidationErrorExpired != 0 && exp != nil {
		if exp.Before(now) && now.Sub(*exp) <= SkewLeeway {
			errors &^= jwt.ValidationErrorExpired
		}
	}

	if errors == 0 {
		return nil
	}

	newErr := *ve
	newErr.Errors = errors
	return &newErr
}

func extractClaimTimes(claims jwt.Claims) (nbf, iat, exp *time.Time) {
	switch c := claims.(type) {
	case *jwt.RegisteredClaims:
		if c.NotBefore != nil {
			t := c.NotBefore.Time
			nbf = &t
		}
		if c.IssuedAt != nil {
			t := c.IssuedAt.Time
			iat = &t
		}
		if c.ExpiresAt != nil {
			t := c.ExpiresAt.Time
			exp = &t
		}
	case *jwt.StandardClaims:
		if c.NotBefore != 0 {
			t := time.Unix(c.NotBefore, 0).UTC()
			nbf = &t
		}
		if c.IssuedAt != 0 {
			t := time.Unix(c.IssuedAt, 0).UTC()
			iat = &t
		}
		if c.ExpiresAt != 0 {
			t := time.Unix(c.ExpiresAt, 0).UTC()
			exp = &t
		}
	case jwt.MapClaims:
		if val, ok := c["nbf"].(float64); ok {
			t := time.Unix(int64(val), 0).UTC()
			nbf = &t
		}
		if val, ok := c["iat"].(float64); ok {
			t := time.Unix(int64(val), 0).UTC()
			iat = &t
		}
		if val, ok := c["exp"].(float64); ok {
			t := time.Unix(int64(val), 0).UTC()
			exp = &t
		}
	case *jwt.MapClaims:
		return extractClaimTimes(jwt.MapClaims(*c))
	}
	return
}
