package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/template"

	petname "github.com/dustinkirkland/golang-petname"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
)

var USER_INFO_FILE = ""
var ZETUP_CONFIG_DIR = ""
var ZETUP_INSTALLATION_ID = ""
var HOMEDIR = ""

// Note: will support gitlab when https://gitlab.com/gitlab-org/gitlab-ce/issues/27954 goes through

// maintain symbolic link to
// git repo
func ZetupLinux() {
	// get sudo privileges
	cmd := exec.Command("sudo", "echo", "have sudo privileges")
	err := cmd.Start()
	if err != nil {
		log.Fatal(err)
	}
	err = cmd.Wait()

	// create unique installation ID
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	username := os.Getenv("USER")
	idNum := petname.Generate(3, "-")
	ZETUP_INSTALLATION_ID = fmt.Sprintf("zetup %v %v %v", hostname, username, idNum)

	// create directories
	HOMEDIR, err = os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	ZETUP_BACKUP_DIR := fmt.Sprintf("%v/.zetup/.bak", HOMEDIR)
	err = os.MkdirAll(ZETUP_BACKUP_DIR, 0755)
	if err != nil {
		log.Fatal(err)
	}
	ZETUP_CONFIG_DIR = fmt.Sprintf("%v/.config/zetup", HOMEDIR)
	err = os.MkdirAll(ZETUP_CONFIG_DIR, 0755)
	if err != nil {
		log.Fatal(err)
	}
	githubToken := getToken()
	USER_INFO_FILE = fmt.Sprintf("%v/user_info.json", ZETUP_CONFIG_DIR)
	userInfo := getUserInfo(githubToken)
	writeGitConfig(userInfo)
	ensureSSHKey(userInfo.Username, githubToken)
}

func ensureSSHKey(username string, githubToken string) {
	PUBLIC_KEY_FILE := fmt.Sprintf("%v/.ssh/id_rsa.pub", HOMEDIR)
	PRIVATE_KEY_FILE := fmt.Sprintf("%v/.ssh/id_rsa", HOMEDIR)
	if _, err := os.Stat(PUBLIC_KEY_FILE); err == nil {
		return // already created public key, assume it's on github
	}

	bitSize := 4096

	privateKey, err := generatePrivateKey(bitSize)
	if err != nil {
		log.Fatal(err.Error())
	}

	publicKeyBytes, err := generatePublicKey(&privateKey.PublicKey)
	if err != nil {
		log.Fatal(err.Error())
	}

	privateKeyBytes := encodePrivateKeyToPEM(privateKey)

	err = writeKeyToFile(privateKeyBytes, PRIVATE_KEY_FILE)
	if err != nil {
		log.Fatal(err.Error())
	}

	err = writeKeyToFile([]byte(publicKeyBytes), PUBLIC_KEY_FILE)
	if err != nil {
		log.Fatal(err.Error())
	}
	addPublicKeyToGithub(string(publicKeyBytes), username, githubToken)
}

func addPublicKeyToGithub(pubKey string, username string, githubToken string) {
	body := strings.NewReader(fmt.Sprintf(`{
				"title": "%v",
				"key": "%v"
			}`, ZETUP_INSTALLATION_ID, strings.TrimRight(pubKey, "\n")))
	req, err := http.NewRequest("POST", "https://api.github.com/user/keys", body)
	if err != nil {
		log.Fatal(err)
	}

	req.SetBasicAuth(username, githubToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	log.Printf("resp = %+v\n", resp)
	if err != nil {
		log.Fatal(err)
	}

	defer resp.Body.Close()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func writeGitConfig(userInfo UserInfo) {
	fileTmpl := `[user]
	name = {{.Name}}
	email = {{.Email}}
`
	t := template.Must(template.New("tgitconfig").Parse(fileTmpl))
	var tplBuff bytes.Buffer

	check(t.Execute(&tplBuff, userInfo))
	_ = ioutil.WriteFile(fmt.Sprintf("%v/.gitconfig", HOMEDIR), []byte(tplBuff.String()), 0644)
}

type UserInfo struct {
	Username string `json:"login"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

func getUserInfo(githubToken string) UserInfo {
	var userInfo UserInfo

	// if user info file exists, use that
	if _, err := os.Stat(USER_INFO_FILE); err == nil {
		jsonFile, err := os.Open(USER_INFO_FILE)
		if err != nil {
			log.Fatal(err)
		}
		byteValue, _ := ioutil.ReadAll(jsonFile)
		json.Unmarshal(byteValue, &userInfo)
		return userInfo
	}

	// get info with personal access token

	// Generated by curl-to-Go: https://mholt.github.io/curl-to-go

	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		log.Fatal(err)
	}
	tokenHeader := fmt.Sprintf("token %v", githubToken)
	log.Printf("tokenHeader = %+v\n", tokenHeader)
	req.Header.Set("Authorization", tokenHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}

	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&userInfo)
	if err != nil {
		log.Fatal(err)
	}

	// write token to file
	file, _ := json.MarshalIndent(userInfo, "", " ")
	_ = ioutil.WriteFile(USER_INFO_FILE, file, 0644)
	return userInfo

}

type TokenInfo struct {
	Id    int    `json:"id"`
	Token string `json:"token"`
}

var TOKEN_INFO_FILE string

func getToken() string {
	// if the github token environment variable is set use that
	envVar := os.Getenv("ZETUP_GITHUB_TOKEN")
	if len(envVar) > 0 {
		return envVar
	}

	// if the token info file exists, parse that
	TOKEN_INFO_FILE = fmt.Sprintf("%v/github_personal_access_token_info.json", ZETUP_CONFIG_DIR)
	if _, err := os.Stat(TOKEN_INFO_FILE); err == nil {
		jsonFile, err := os.Open(TOKEN_INFO_FILE)
		if err != nil {
			log.Fatal(err)
		}
		byteValue, _ := ioutil.ReadAll(jsonFile)
		var tokenInfo TokenInfo
		json.Unmarshal(byteValue, &tokenInfo)
		return tokenInfo.Token
	}

	// no token present, so create
	return createToken()
}

type TokenPayload struct {
	Note   string   `json:"note"`
	Scopes []string `json:"scopes"`
}

func createToken() string {
	// get github username and password
	username := os.Getenv("GITHUB_USERNAME")
	if len(username) == 0 {
		reader := bufio.NewReader(os.Stdin)
		username = os.Getenv("USER")
		fmt.Printf("Github Username (%v): ", username)
		enteredUsername, err := reader.ReadString('\n')
		enteredUsername = strings.Trim(enteredUsername, " ")
		if err != nil {
			log.Fatal(err)
		}
	} else {
		fmt.Println("Using Github Username ", username)
	}

	password := os.Getenv("GITHUB_PASSWORD")
	if len(password) == 0 {
		password = getPassword("Github Password: ")
	}

	// send token request
	data := TokenPayload{
		Note: ZETUP_INSTALLATION_ID,
		Scopes: []string{
			"repo",
			"admin:org",
			"admin:public_key",
			"admin:repo_hook",
			"gist",
			"notifications",
			"user",
			"delete_repo",
			"write:discussion",
			"admin:gpg_key",
		},
	}
	payloadBytes, err := json.Marshal(data)
	if err != nil {
		log.Fatal(err)
	}
	body := bytes.NewReader(payloadBytes)

	req, err := http.NewRequest("POST", "https://api.github.com/authorizations", body)
	if err != nil {
		log.Fatal(err)
	}

	req.SetBasicAuth(username, password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}

	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var respTokenData TokenInfo
	err = decoder.Decode(&respTokenData)
	if err != nil {
		log.Fatal(err)
	}

	// write token to file
	file, _ := json.MarshalIndent(respTokenData, "", " ")
	_ = ioutil.WriteFile(TOKEN_INFO_FILE, file, 0644)
	return respTokenData.Token
}

/*
* Not my code ↓
 */
func getPassword(prompt string) string {
	// Get the initial state of the terminal.
	initialTermState, e1 := terminal.GetState(syscall.Stdin)
	if e1 != nil {
		panic(e1)
	}

	// Restore it in the event of an interrupt.
	// CITATION: Konstantin Shaposhnikov - https://groups.google.com/forum/#!topic/golang-nuts/kTVAbtee9UA

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, os.Kill)
	go func() {
		<-c
		_ = terminal.Restore(syscall.Stdin, initialTermState)
		os.Exit(1)
	}()

	// Now get the password.
	fmt.Print(prompt)
	p, err := terminal.ReadPassword(syscall.Stdin)
	fmt.Println("")
	if err != nil {
		panic(err)
	}

	// Stop looking for ^C on the channel.
	signal.Stop(c)

	// Return the password as a string.
	return string(p)
}

// ↓ https://gist.github.com/devinodaniel/8f9b8a4f31573f428f29ec0e884e6673
// generatePrivateKey creates a RSA Private Key of specified byte size
func generatePrivateKey(bitSize int) (*rsa.PrivateKey, error) {
	// Private Key generation
	privateKey, err := rsa.GenerateKey(rand.Reader, bitSize)
	if err != nil {
		return nil, err
	}

	// Validate Private Key
	err = privateKey.Validate()
	if err != nil {
		return nil, err
	}

	return privateKey, nil
}

// encodePrivateKeyToPEM encodes Private Key from RSA to PEM format
func encodePrivateKeyToPEM(privateKey *rsa.PrivateKey) []byte {
	// Get ASN.1 DER format
	privDER := x509.MarshalPKCS1PrivateKey(privateKey)

	// pem.Block
	privBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   privDER,
	}

	// Private key in PEM format
	privatePEM := pem.EncodeToMemory(&privBlock)

	return privatePEM
}

// generatePublicKey take a rsa.PublicKey and return bytes suitable for writing to .pub file
// returns in the format "ssh-rsa ..."
func generatePublicKey(privatekey *rsa.PublicKey) ([]byte, error) {
	publicRsaKey, err := ssh.NewPublicKey(privatekey)
	if err != nil {
		return nil, err
	}

	pubKeyBytes := ssh.MarshalAuthorizedKey(publicRsaKey)

	return pubKeyBytes, nil
}

// writePemToFile writes keys to a file
func writeKeyToFile(keyBytes []byte, saveFileTo string) error {
	err := ioutil.WriteFile(saveFileTo, keyBytes, 0600)
	if err != nil {
		return err
	}

	return nil
}
