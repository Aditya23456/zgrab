/*
 * ZGrab Copyright 2015 Regents of the University of Michigan
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy
 * of the License at http://www.apache.org/licenses/LICENSE-2.0
 */

package keys

import (
	"crypto/rsa"
	"encoding/json"
	"math/big"
)

type RSAPublicKey struct {
	*rsa.PublicKey
}

type auxRSAPublicKey struct {
	Exponent int    `json:"exponent"`
	Modulus  []byte `json:"modulus"`
}

// MarshalJSON implements the json.Marshal interface
func (rp *RSAPublicKey) MarshalJSON() ([]byte, error) {
	var aux auxRSAPublicKey
	if rp.PublicKey != nil {
		aux.Exponent = rp.E
		aux.Modulus = rp.N.Bytes()
	}
	return json.Marshal(&aux)
}

// UnmarshalJSON implements teh json.Unmarshal interface
func (rp *RSAPublicKey) UnmarshalJSON(b []byte) error {
	var aux auxRSAPublicKey
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if rp.PublicKey == nil {
		rp.PublicKey = new(rsa.PublicKey)
	}
	rp.E = aux.Exponent
	rp.N = big.NewInt(0).SetBytes(aux.Modulus)
	return nil
}
