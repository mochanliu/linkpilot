package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"

	"fmt"
)

func main() {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	fmt.Println(privateKey)
	publicKey := privateKey.PublicKey
	encryptedBytes, err := rsa.EncryptOAEP(
		sha256.New(),
		rand.Reader,
		&publicKey,
		[]byte("2023 5 1"),
		nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("encrypted bytes: ", encryptedBytes)
}
