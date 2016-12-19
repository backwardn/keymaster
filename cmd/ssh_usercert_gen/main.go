package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"github.com/Symantec/Dominator/lib/logbuf"
	"github.com/Symantec/keymaster/lib/authutil"
	"github.com/Symantec/keymaster/lib/certgen"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v2"
	//"io"
	"io/ioutil"
	"log"
	//"net"
	"net/http"
	//"net/url"
	"os"
	"regexp"
	//"strconv"
	"strings"
	//"sync"
	//"time"
)

// describes the network config and the mechanism for user auth.
// While the contents of the certificaes are public, we want to
// restrict generation to authenticated users
type baseConfig struct {
	Http_Address      string
	TLS_Cert_Filename string
	TLS_Key_Filename  string
	UserAuth          string
	SSH_CA_Filename   string
	Htpasswd_Filename string
}

type LdapConfig struct {
	Bind_Pattern     string
	LDAP_Target_URLs string
}

type AppConfigFile struct {
	Base baseConfig
	Ldap LdapConfig
}

type RuntimeState struct {
	Config       AppConfigFile
	Signer       ssh.Signer
	HostIdentity string
}

var (
	configFilename = flag.String("config", "config.yml", "The filename of the configuration")
	debug          = flag.Bool("debug", false, "Enable debug messages to console")
)

func getHostIdentity() (string, error) {
	return os.Hostname()
}

func exitsAndCanRead(fileName string, description string) error {
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		err = errors.New("mising " + description + " file")
		return err
	}
	_, err := ioutil.ReadFile(fileName)
	if err != nil {
		err = errors.New("cannot read " + description + "file")
		return err
	}
	return nil
}

func loadVerifyConfigFile(configFilename string) (RuntimeState, error) {
	var runtimeState RuntimeState
	if _, err := os.Stat(configFilename); os.IsNotExist(err) {
		err = errors.New("mising config file failure")
		return runtimeState, err
	}
	source, err := ioutil.ReadFile(configFilename)
	if err != nil {
		err = errors.New("cannot read config file")
		return runtimeState, err
	}
	err = yaml.Unmarshal(source, &runtimeState.Config)
	if err != nil {
		err = errors.New("Cannot parse config file")
		return runtimeState, err
	}

	//verify config
	sshCAFilename := runtimeState.Config.Base.SSH_CA_Filename
	err = exitsAndCanRead(sshCAFilename, "ssh CA File")
	if err != nil {
		return runtimeState, err
	}
	// load private key and make signer
	buffer, err := ioutil.ReadFile(sshCAFilename)
	if err != nil {
		return runtimeState, err
	}
	runtimeState.Signer, err = ssh.ParsePrivateKey(buffer)
	if err != nil {
		log.Printf("Cannot parse Priave Key file")
		return runtimeState, err
	}

	err = exitsAndCanRead(runtimeState.Config.Base.TLS_Cert_Filename, "http cert file")
	if err != nil {
		return runtimeState, err
	}
	err = exitsAndCanRead(runtimeState.Config.Base.TLS_Key_Filename, "http key file")
	if err != nil {
		return runtimeState, err
	}

	runtimeState.HostIdentity, err = getHostIdentity()
	if err != nil {
		return runtimeState, err
	}

	return runtimeState, nil
}

func convertToBindDN(username string, bind_pattern string) string {
	return fmt.Sprintf(bind_pattern, username)
}

func checkUserPassword(username string, password string, config AppConfigFile) (bool, error) {
	//if username == "camilo_viecco1" && password == "pass" {
	//	return true, nil
	//}

	const timeoutSecs = 3
	bindDN := convertToBindDN(username, config.Ldap.Bind_Pattern)
	for _, ldapUrl := range strings.Split(config.Ldap.LDAP_Target_URLs, ",") {
		u, err := authutil.ParseLDAPURL(ldapUrl)
		if err != nil {
			log.Printf("Failed to parse %s", ldapUrl)
			continue
		}
		vaild, err := authutil.CheckLDAPUserPassword(*u, bindDN, password, timeoutSecs)
		if err != nil {
			//log.Printf("Failed to parse %s", ldapUrl)
			continue
		}
		// the ldap exchange was successful (user might be invaid)
		return vaild, nil

	}
	if config.Base.Htpasswd_Filename != "" {
		log.Printf("I have htpasswed filename")
		buffer, err := ioutil.ReadFile(config.Base.Htpasswd_Filename)
		if err != nil {
			return false, err
		}
		valid, err := authutil.CheckHtpasswdUserPassword(username, password, buffer)
		if err != nil {
			return false, err
		}
		return valid, nil
	}
	return false, nil
}

func writeFailureResponse(w http.ResponseWriter, code int, message string) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="User Credentials"`)
	}
	w.WriteHeader(code)
	publicErrorText := fmt.Sprintf("%d %s %s\n", code, http.StatusText(code), message)
	w.Write([]byte(publicErrorText))
}

// Inspired by http://stackoverflow.com/questions/21936332/idiomatic-way-of-requiring-http-basic-auth-in-go
func checkAuth(w http.ResponseWriter, r *http.Request, config AppConfigFile) (string, error) {
	//For now just check http basic
	user, pass, ok := r.BasicAuth()
	if !ok {
		writeFailureResponse(w, http.StatusUnauthorized, "")
		err := errors.New("check_Auth, Invalid or no auth header")
		return "", err
	}
	valid, err := checkUserPassword(user, pass, config)
	if err != nil {
		writeFailureResponse(w, http.StatusInternalServerError, "")
		return "", err
	}
	if !valid {
		writeFailureResponse(w, http.StatusUnauthorized, "")
		err := errors.New("Invalid Credentials")
		return "", err
	}
	return user, nil

}

const CERTGEN_PATH = "/certgen/"

func (state RuntimeState) certGenHandler(w http.ResponseWriter, r *http.Request) {
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	authUser, err := checkAuth(w, r, state.Config)
	if err != nil {
		log.Printf("%v", err)

		return
	}

	targetUser := r.URL.Path[len(CERTGEN_PATH):]
	if authUser != targetUser {
		writeFailureResponse(w, http.StatusForbidden, "")
		log.Printf("User %s asking for creds for %s", authUser, targetUser)
		return
	}
	if *debug {
		log.Printf("auth succedded for %s", authUser)
	}

	var cert string
	switch r.Method {
	case "GET":
		cert, err = certgen.GenSSHCertFileStringFromSSSDPublicKey(targetUser, state.Signer, state.HostIdentity)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	case "POST":
		if *debug {
			log.Printf("Got client POST connection")
		}
		err = r.ParseMultipartForm(1e7)
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, http.StatusBadRequest, "Error parsing form")
			return
		}

		file, _, err := r.FormFile("pubkeyfile")
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, http.StatusBadRequest, "Missing public key file")
			return
		}
		defer file.Close()
		buf := new(bytes.Buffer)
		buf.ReadFrom(file)
		userPubKey := buf.String()
		//validKey, err := regexp.MatchString("^(ssh-rsa|ssh-dss|ecdsa-sha2-nistp256|ssh-ed25519) [a-zA-Z0-9/+]+=?=? .*$", userPubKey)
		validKey, err := regexp.MatchString("^(ssh-rsa|ssh-dss|ecdsa-sha2-nistp256|ssh-ed25519) [a-zA-Z0-9/+]+=?=? ?.{0,512}\n?$", userPubKey)
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, http.StatusInternalServerError, "")
			return
		}
		if !validKey {
			writeFailureResponse(w, http.StatusBadRequest, "Invalid File, bad re")
			log.Printf("invalid file, bad re")
			return

		}

		cert, err = certgen.GenSSHCertFileString(targetUser, userPubKey, state.Signer, state.HostIdentity)
		if err != nil {
			writeFailureResponse(w, http.StatusInternalServerError, "")
			log.Printf("signUserPubkey Err")
			return
		}

	default:
		writeFailureResponse(w, http.StatusMethodNotAllowed, "")
		return

	}
	w.Header().Set("Content-Disposition", `attachment; filename="id_rsa-cert.pub"`)
	w.WriteHeader(200)
	fmt.Fprintf(w, "%s", cert)
	log.Printf("Generated Certifcate for %s", targetUser)
}

func main() {
	flag.Parse()

	circularBuffer := logbuf.New()
	if circularBuffer == nil {
		panic("Cannot create circular buffer")
	}
	log.New(circularBuffer, "", log.LstdFlags)

	runtimeState, err := loadVerifyConfigFile(*configFilename)
	if err != nil {
		panic(err)
	}
	if *debug || true {
		log.Printf("After load verify")
	}

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", prometheus.Handler())
	http.HandleFunc(CERTGEN_PATH, runtimeState.certGenHandler)

	cfg := &tls.Config{
		ClientAuth:               tls.RequestClientCert,
		MinVersion:               tls.VersionTLS12,
		CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		},
	}
	srv := &http.Server{
		Addr:         runtimeState.Config.Base.Http_Address,
		TLSConfig:    cfg,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler), 0),
	}

	err = srv.ListenAndServeTLS(
		runtimeState.Config.Base.TLS_Cert_Filename,
		runtimeState.Config.Base.TLS_Key_Filename)
	if err != nil {
		panic(err)
	}
}