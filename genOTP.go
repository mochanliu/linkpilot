package main

import (
	"fmt"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

/* func promptForPasscode() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter Passcode: ")
	text, _ := reader.ReadString('\n')
	return text
}

func GeneratePassCode(utf8string string) string {
	secret := base32.StdEncoding.EncodeToString([]byte(utf8string))
	passcode, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA512,
	})
	if err != nil {
		panic(err)
	}
	return passcode
} */

func main() {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "wonderfulnet.cn",
		AccountName: "louis@wonderfulnet.cn",
		SecretSize:  256,
		Algorithm:   otp.AlgorithmSHA512,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(key.Secret())
	/* 	fmt.Println("Validating TOTP...")
	   	passcode := promptForPasscode()
	   	valid := totp.Validate(passcode, key.Secret())
	   	if valid {
	   		println("Valid passcode!")
	   		os.Exit(0)
	   	} else {
	   		println("Invalid passcode!")
	   		os.Exit(1)
	   	} */
}
