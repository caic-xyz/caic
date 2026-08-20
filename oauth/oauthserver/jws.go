// JWS signing and verification per RFC 7515 and RFC 7518.

package oauthserver

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha256" // registers SHA-256 for crypto.Hash.New
	_ "crypto/sha512" // registers SHA-384/SHA-512 for crypto.Hash.New
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// The signing scheme always comes from the JOSE "alg", never from the key type:
// a key type alone does not distinguish RS* from PS*, nor say which digest a
// curve uses. Callers bind alg to the key before reaching these helpers —
// checkAlgKeyType for client-supplied DPoP proof keys, the signingKey.alg
// recorded at key creation for the server's own access tokens.

// joseHash returns the digest algorithm bound to a JOSE alg per RFC 7518 §3.1.
//
// EdDSA has no separate digest: Ed25519 hashes the message internally
// (RFC 8037 §3.1), so it maps to the zero crypto.Hash.
func joseHash(alg string) (crypto.Hash, error) {
	switch alg {
	case "RS256", "PS256", "ES256":
		return crypto.SHA256, nil
	case "RS384", "PS384", "ES384":
		return crypto.SHA384, nil
	case "RS512", "PS512", "ES512":
		return crypto.SHA512, nil
	case "EdDSA":
		return 0, nil
	default:
		return 0, fmt.Errorf("unsupported jws alg: %s", alg)
	}
}

// ecdsaAlg returns the ES* alg bound to a curve per RFC 7518 §3.4.
//
// The pairing is fixed: a curve determines its digest, so P-521 is ES512 and
// never ES256.
func ecdsaAlg(curve elliptic.Curve) (string, error) {
	switch curve {
	case elliptic.P256():
		return "ES256", nil
	case elliptic.P384():
		return "ES384", nil
	case elliptic.P521():
		return "ES512", nil
	default:
		return "", fmt.Errorf("unsupported ec curve: %s", curve.Params().Name)
	}
}

// ecdsaCurve returns the curve named by a JWK "crv" per RFC 7518 §6.2.1.1.
func ecdsaCurve(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("unsupported ec curve: %s", crv)
	}
}

// coordinateSize returns the byte length of one curve coordinate, which is also
// the length of each half of an ES* signature (RFC 7518 §3.4, §6.2.1.2).
func coordinateSize(curve elliptic.Curve) int {
	return (curve.Params().BitSize + 7) / 8
}

// signJWS signs a JWS signing input with alg per RFC 7518 §3.3-§3.5.
func signJWS(alg string, signer crypto.Signer, signingInput []byte) ([]byte, error) {
	if ed, ok := signer.(ed25519.PrivateKey); ok {
		// RFC 8037 §3.1: EdDSA signs the message directly, with no prehash.
		return ed25519.Sign(ed, signingInput), nil
	}
	hash, err := joseHash(alg)
	if err != nil {
		return nil, err
	}
	digest := hashSigningInput(hash, signingInput)
	switch key := signer.(type) {
	case *rsa.PrivateKey:
		// RFC 7518 §3.5: PS* is RSASSA-PSS, with MGF1 using the same digest and a
		// salt the length of that digest. §3.3: RS* is RSASSA-PKCS1-v1_5.
		if strings.HasPrefix(alg, "PS") {
			opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: hash}
			return rsa.SignPSS(rand.Reader, key, hash, digest, opts)
		}
		return rsa.SignPKCS1v15(rand.Reader, key, hash, digest)
	case *ecdsa.PrivateKey:
		// RFC 7518 §3.4: emit the raw R||S concatenation, each coordinate
		// left-padded to the curve's byte size. It is not ASN.1/DER.
		want, err := ecdsaAlg(key.Curve)
		if err != nil {
			return nil, err
		}
		if alg != want {
			return nil, fmt.Errorf("alg %s does not match curve %s (want %s)", alg, key.Curve.Params().Name, want)
		}
		r, s, err := ecdsa.Sign(rand.Reader, key, digest)
		if err != nil {
			return nil, fmt.Errorf("ecdsa sign: %w", err)
		}
		size := coordinateSize(key.Curve)
		sig := make([]byte, 2*size)
		r.FillBytes(sig[:size])
		s.FillBytes(sig[size:])
		return sig, nil
	default:
		return nil, fmt.Errorf("unsupported signing key type: %T", signer)
	}
}

// verifyJWS verifies a JWS signature with alg per RFC 7515 §5.2.
func verifyJWS(pub crypto.PublicKey, alg string, signingInput, signature []byte) error {
	hash, err := joseHash(alg)
	if err != nil {
		return err
	}
	switch key := pub.(type) {
	case *rsa.PublicKey:
		digest := hashSigningInput(hash, signingInput)
		if strings.HasPrefix(alg, "PS") {
			opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: hash}
			if err := rsa.VerifyPSS(key, hash, digest, signature, opts); err != nil {
				return fmt.Errorf("rsa-pss signature verification failed: %w", err)
			}
			return nil
		}
		if err := rsa.VerifyPKCS1v15(key, hash, digest, signature); err != nil {
			return fmt.Errorf("rsa signature verification failed: %w", err)
		}
		return nil
	case *ecdsa.PublicKey:
		want, err := ecdsaAlg(key.Curve)
		if err != nil {
			return err
		}
		if alg != want {
			return fmt.Errorf("alg %s does not match curve %s (want %s)", alg, key.Curve.Params().Name, want)
		}
		size := coordinateSize(key.Curve)
		if len(signature) != 2*size {
			return fmt.Errorf("ecdsa signature must be %d bytes for %s, got %d", 2*size, alg, len(signature))
		}
		r := new(big.Int).SetBytes(signature[:size])
		s := new(big.Int).SetBytes(signature[size:])
		if !ecdsa.Verify(key, hashSigningInput(hash, signingInput), r, s) {
			return errors.New("ecdsa signature verification failed")
		}
		return nil
	case ed25519.PublicKey:
		if !ed25519.Verify(key, signingInput, signature) {
			return errors.New("ed25519 signature verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unsupported public key type: %T", pub)
	}
}

// hashSigningInput digests the JWS signing input with the alg's hash.
func hashSigningInput(hash crypto.Hash, signingInput []byte) []byte {
	h := hash.New()
	h.Write(signingInput)
	return h.Sum(nil)
}
