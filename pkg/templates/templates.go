// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/golang/glog"
	"github.com/spf13/cast"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultStartDelim = "{{"
	defaultStopDelim  = "}}"
	IVSize            = 16 // Size in bytes
	glogDefLvl        = 2
	protectedPrefix   = "$ocm_encrypted:"
	yamlIndentation   = 2
)

type EncryptionMode uint8

const (
	// Disables the "protect" template function.
	EncryptionDisabled EncryptionMode = iota
	// Enables the "protect" template function and "fromSecret" returns encrypted content.
	EncryptionEnabled
	// Enables automatic decrypting of encrypted strings. With this mode, the "protect" template function is not
	// available and "fromSecret" does not return encrypted content.
	DecryptionEnabled
)

var (
	ErrAESKeyNotSet          = errors.New("AESKey must be set to use this encryption mode")
	ErrInvalidAESKey         = errors.New("the AES key is invalid")
	ErrInvalidB64OfEncrypted = errors.New("the encrypted string is invalid base64")
	// nolint: golint
	ErrInvalidIV           = errors.New("InitializationVector must be 128 bits")
	ErrInvalidPKCS7Padding = errors.New("invalid PCKS7 padding")
	ErrProtectNotEnabled   = errors.New("the protect template function is not enabled in this mode")
)

// Config is a struct containing configuration for the API. Some are required.
//
// - AdditionalIndentation sets the number of additional spaces to be added to the input number
// to the indent method. This is useful in situations when the indentation should be relative
// to a logical starting point in a YAML file.
//
// - AESKey is an AES key (e.g. AES-256) to use for the "protect" template function and decrypting
// such values. If it's not specified, the "protect" template function will be undefined.
//
// - DecryptionConcurrency is the concurrency (i.e. number of Goroutines) limit when decrypting encrypted strings. Not
// setting this value is the equivalent of setting this to 1, which means no concurrency.
//
// - DisabledFunctions is a slice of default template function names that should be disabled.
// - KubeAPIResourceList sets the cache for the Kubernetes API resources. If this is
// set, template processing will not try to rediscover the Kubernetes API resources
// needed for dynamic client/ GVK lookups.
//
// - EncryptionMode determines the encryption mode to use. See the package-level EncryptionMode variables to choose
// from.
//
// - InitializationVector is the initialization vector (IV) used in the AES-CBC encryption/decryption. Note that it must
// be equal to the AES block size which is always 128 bits (16 bytes). This value must be random but does not need to be
// private. Its purpose is to make the same plaintext value, when encrypted with the same AES key, appear unique. When
// performing decryption, the IV must be the same as it was for the encryption of the data. Note that all values
// encrypted in the template will use this same IV, which means that duplicate plaintext values that are encrypted will
// yield the same encrypted value in the template.
//
// - LookupNamespace is the namespace to restrict "lookup" template functions (e.g. fromConfigMap)
// to. If this is not set (i.e. an empty string), then all namespaces can be used.
//
// - StartDelim customizes the start delimiter used to distinguish a template action. This defaults
// to "{{". If StopDelim is set, this must also be set.
//
// - StopDelim customizes the stop delimiter used to distinguish a template action. This defaults
// to "}}". If StartDelim is set, this must also be set.
type Config struct {
	AdditionalIndentation uint
	AESKey                []byte
	DecryptionConcurrency uint8
	DisabledFunctions     []string
	EncryptionMode        EncryptionMode
	InitializationVector  []byte
	KubeAPIResourceList   []*metav1.APIResourceList
	LookupNamespace       string
	StartDelim            string
	StopDelim             string
}

// TemplateResolver is the API for processing templates. It's better to use the NewResolver function
// instead of instantiating this directly so that configuration defaults and validation are applied.
type TemplateResolver struct {
	kubeClient *kubernetes.Interface
	kubeConfig *rest.Config
	config     Config
}

// NewResolver creates a new TemplateResolver instance, which is the API for processing templates.
//
// - kubeClient is the Kubernetes client to be used for the template lookup functions.
//
// - config is the Config instance for configuration for template processing.
func NewResolver(kubeClient *kubernetes.Interface, kubeConfig *rest.Config, config Config) (*TemplateResolver, error) {
	if kubeClient == nil {
		return nil, fmt.Errorf("kubeClient must be a non-nil value")
	}

	if config.EncryptionMode == EncryptionEnabled || config.EncryptionMode == DecryptionEnabled {
		if config.AESKey == nil {
			return nil, ErrAESKeyNotSet
		}

		// AES uses a 128 bit (16 byte) block size no matter the key size. The initialization vector must be the same
		// length as the block size.
		if len(config.InitializationVector) != IVSize {
			return nil, ErrInvalidIV
		}
	}

	if (config.StartDelim != "" && config.StopDelim == "") || (config.StartDelim == "" && config.StopDelim != "") {
		return nil, fmt.Errorf("the configurations StartDelim and StopDelim cannot be set independently")
	}

	// It's only required to check config.StartDelim since it's invalid to set these independently
	if config.StartDelim == "" {
		config.StartDelim = defaultStartDelim
		config.StopDelim = defaultStopDelim
	}

	glog.V(glogDefLvl).Infof("Using the delimiters of %s and %s", config.StartDelim, config.StopDelim)

	return &TemplateResolver{
		kubeClient: kubeClient,
		kubeConfig: kubeConfig,
		config:     config,
	}, nil
}

// HasTemplate performs a simple check for the template start delimiter or the "$ocm_encrypted" prefix
// (checkForEncrypted must be set to true) to indicate if the input byte slice has a template. If the startDelim
// argument is an empty string, the default start delimiter of "{{" will be used.
func HasTemplate(template []byte, startDelim string, checkForEncrypted bool) bool {
	if startDelim == "" {
		startDelim = defaultStartDelim
	}

	templateStr := string(template)
	glog.V(glogDefLvl).Infof("HasTemplate template str:  %v", templateStr)
	glog.V(glogDefLvl).Infof("Checking for the start delimiter:  %s", startDelim)

	hasTemplate := false
	if strings.Contains(templateStr, startDelim) {
		hasTemplate = true
	} else if checkForEncrypted && strings.Contains(templateStr, protectedPrefix) {
		hasTemplate = true
	}

	glog.V(glogDefLvl).Infof("hasTemplate: %v", hasTemplate)

	return hasTemplate
}

// getValidContext takes an input context struct with string fields and
// validates it. If is is valid, the context will be returned as is. If the
// input context is nil, an empty struct will be returned. If it's not valid, an
// error will be returned.
func getValidContext(context interface{}) (ctx interface{}, _ error) {
	var ctxType reflect.Type

	if context == nil {
		ctx = struct{}{}

		return ctx, nil
	}

	ctxType = reflect.TypeOf(context)
	if ctxType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("the input context must be a struct with string fields, got %s", ctxType)
	}

	for i := 0; i < ctxType.NumField(); i++ {
		f := ctxType.Field(i)
		if f.Type.Kind() != reflect.String {
			return nil, errors.New("the input context must be a struct with string fields")
		}
	}

	return context, nil
}

// ResolveTemplate accepts a map marshaled as JSON. It also accepts a struct
// with string fields that will be made available when the template is processed.
// For example, if the argument is `struct{ClusterName string}{"cluster1"}`,
// the value `cluster1` would be available with `{{ .ClusterName }}`. This can
// also be `nil` if no fields should be made available.
//
// ResolveTemplate will process any template strings in the map and return the processed map.
func (t *TemplateResolver) ResolveTemplate(tmplJSON []byte, context interface{}) ([]byte, error) {
	glog.V(glogDefLvl).Infof("ResolveTemplate for: %v", tmplJSON)

	ctx, err := getValidContext(context)
	if err != nil {
		return []byte(""), err
	}

	// Build Map of supported template functions
	funcMap := template.FuncMap{
		"fromSecret":       t.fromSecret,
		"fromConfigMap":    t.fromConfigMap,
		"fromClusterClaim": t.fromClusterClaim,
		"lookup":           t.lookup,
		"base64enc":        base64encode,
		"base64dec":        base64decode,
		"autoindent":       autoindent,
		"indent":           t.indent,
		"atoi":             atoi,
		"toInt":            toInt,
		"toBool":           toBool,
	}

	if t.config.EncryptionMode == EncryptionEnabled {
		funcMap["fromSecret"] = t.fromSecretProtected
		funcMap["protect"] = t.protect
	} else {
		// In other encryption modes, return a readable error if the protect template function is accidentally used.
		funcMap["protect"] = func(s string) (string, error) { return "", ErrProtectNotEnabled }
	}

	for _, funcName := range t.config.DisabledFunctions {
		delete(funcMap, funcName)
	}

	// create template processor and Initialize function map
	tmpl := template.New("tmpl").Delims(t.config.StartDelim, t.config.StopDelim).Funcs(funcMap)

	// convert the JSON to YAML
	templateYAMLBytes, err := jsonToYAML(tmplJSON)
	if err != nil {
		return []byte(""), fmt.Errorf("failed to convert the policy template to YAML: %w", err)
	}

	templateStr := string(templateYAMLBytes)
	glog.V(glogDefLvl).Infof("Initial template str to resolve : %v ", templateStr)

	if t.config.EncryptionMode == DecryptionEnabled {
		templateStr, err = t.processEncryptedStrs(templateStr)
		if err != nil {
			return nil, err
		}
	}

	// process for int or bool
	if strings.Contains(templateStr, "toInt") || strings.Contains(templateStr, "toBool") {
		templateStr = t.processForDataTypes(templateStr)
	}

	// convert `autoindent` placeholders to `indent N`
	if strings.Contains(templateStr, "autoindent") {
		templateStr = t.processForAutoIndent(templateStr)
	}

	tmpl, err = tmpl.Parse(templateStr)
	if err != nil {
		tmplJSONStr := string(tmplJSON)
		glog.Errorf(
			"error parsing template JSON string %v,\n template str %v,\n error: %v", tmplJSONStr, templateStr, err,
		)

		return []byte(""), fmt.Errorf("failed to parse the template JSON string %v: %w", tmplJSONStr, err)
	}

	var buf strings.Builder

	err = tmpl.Execute(&buf, ctx)
	if err != nil {
		tmplJSONStr := string(tmplJSON)
		glog.Errorf("error resolving the template %v,\n template str %v,\n error: %v", tmplJSONStr, templateStr, err)

		return []byte(""), fmt.Errorf("failed to resolve the template %v: %w", tmplJSONStr, err)
	}

	resolvedTemplateStr := buf.String()
	glog.V(glogDefLvl).Infof("resolved template str: %v ", resolvedTemplateStr)
	// unmarshall before returning

	resolvedTemplateBytes, err := yamlToJSON([]byte(resolvedTemplateStr))
	if err != nil {
		return []byte(""), fmt.Errorf("failed to convert the resolved template back to YAML: %w", err)
	}

	return resolvedTemplateBytes, nil
}

//nolint: wsl
func (t *TemplateResolver) processForDataTypes(str string) string {
	// the idea is to remove the quotes enclosing the template if it ends in toBool ot ToInt
	// quotes around the resolved template forces the value to be a string..
	// so removal of these quotes allows yaml to process the datatype correctly..

	// the below pattern searches for optional block scalars | or >.. followed by the quoted template ,
	// and replaces it with just the template txt thats inside in the quotes
	// ex-1 key : "{{ "6" | toInt }}"  .. is replaced with  key : {{ "6" | toInt }}
	// ex-2 key : |
	//						"{{ "true" | toBool }}" .. is replaced with key : {{ "true" | toBool }}
	d1 := regexp.QuoteMeta(t.config.StartDelim)
	d2 := regexp.QuoteMeta(t.config.StopDelim)
	re := regexp.MustCompile(
		`:\s+(?:[\|>][-]?\s+)?(?:['|"]\s*)?(` + d1 + `.*?\s+\|\s+(?:toInt|toBool)\s*` + d2 + `)(?:\s*['|"])?`,
	)
	glog.V(glogDefLvl).Infof("\n Pattern: %v\n", re.String())

	submatchall := re.FindAllStringSubmatch(str, -1)
	glog.V(glogDefLvl).Infof("\n All Submatches:\n%v", submatchall)

	processeddata := re.ReplaceAllString(str, ": $1")
	glog.V(glogDefLvl).Infof("\n processed data :\n%v", processeddata)

	return processeddata
}

// processForAutoIndent converts any `autoindent` placeholders into `indent N` in the string.
// The processed input string is returned.
func (t *TemplateResolver) processForAutoIndent(str string) string {
	d1 := regexp.QuoteMeta(t.config.StartDelim)
	d2 := regexp.QuoteMeta(t.config.StopDelim)
	// Detect any templates that contain `autoindent` and capture the spaces before it.
	// Later on, the amount of spaces will dictate the conversion of `autoindent` to `indent`.
	// This is not a very strict regex as occasionally, a user will make a mistake such as
	// `config: '{{ "hello\nworld" | autoindent }}'`. In that event, `autoindent` will change to
	// `indent 1`, but `indent` properly handles this.
	re := regexp.MustCompile(`( *)(?:'|")?(` + d1 + `.*\| *autoindent *` + d2 + `)`)
	glog.V(glogDefLvl).Infof("\n Pattern: %v\n", re.String())

	submatches := re.FindAllStringSubmatch(str, -1)
	processed := str

	glog.V(glogDefLvl).Infof("\n All Submatches:\n%v", submatches)

	for _, submatch := range submatches {
		numSpaces := len(submatch[1]) - int(t.config.AdditionalIndentation)
		matchStr := submatch[2]
		newMatchStr := strings.Replace(matchStr, "autoindent", fmt.Sprintf("indent %d", numSpaces), 1)
		processed = strings.Replace(processed, matchStr, newMatchStr, 1)
	}

	glog.V(glogDefLvl).Infof("\n processed data :\n%v", processed)

	return processed
}

// processEncryptedStrs replaces all encrypted strings with the decrypted values. Each decryption is handled
// concurrently and the concurrency limit is controlled by t.config.DecryptionConcurrency. If a decryption fails,
// the rest of the decryption is halted and an error is returned.
func (t *TemplateResolver) processEncryptedStrs(templateStr string) (string, error) {
	// This catching any encrypted string in the format of $ocm_encrypted:<base64 of the encrypted value>.
	re := regexp.MustCompile(regexp.QuoteMeta(protectedPrefix) + "([a-zA-Z0-9+/=]+)")
	// Each submatch will have index 0 be the whole match and index 1 as the base64 of the encrypted value.
	submatches := re.FindAllStringSubmatch(templateStr, -1)

	if len(submatches) == 0 {
		return templateStr, nil
	}

	var numWorkers int

	// Determine how many Goroutines to spawn.
	if t.config.DecryptionConcurrency <= 1 {
		numWorkers = 1
	} else if len(submatches) > int(t.config.DecryptionConcurrency) {
		numWorkers = int(t.config.DecryptionConcurrency)
	} else {
		numWorkers = len(submatches)
	}

	submatchesChan := make(chan []string, len(submatches))
	resultsChan := make(chan decryptResult, len(submatches))

	glog.V(glogDefLvl).Infof("Will decrypt %d value(s) with %d Goroutines", len(submatches), numWorkers)

	// Create a context to be able to cancel decryption in case one fails.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start up all the Goroutines.
	for i := 0; i < numWorkers; i++ {
		go t.decryptWrapper(ctx, submatchesChan, resultsChan)
	}

	// Send all the submatches of all the encrypted strings to the Goroutines to process.
	for _, submatch := range submatches {
		submatchesChan <- submatch
	}

	processed := templateStr
	processedResults := 0

	for result := range resultsChan {
		// If an error occurs, stop the Goroutines and return the error.
		if result.err != nil {
			// Cancel the context so the Goroutines exit before the channels close.
			cancel()
			close(submatchesChan)
			close(resultsChan)
			glog.Errorf("Decryption failed %v", result.err)

			return "", fmt.Errorf("decryption of %s failed: %w", result.match, result.err)
		}

		processed = strings.Replace(processed, result.match, result.plaintext, 1)
		processedResults++

		// Once the decryption is complete, it's safe to close the channels and stop blocking in this Goroutine.
		if processedResults == len(submatches) {
			close(submatchesChan)
			close(resultsChan)
		}
	}

	glog.V(glogDefLvl).Infof("Finished decrypting %d value(s)", len(submatches))

	return processed, nil
}

// decryptResult is the result sent back on the "results" channel in decryptWrapper.
type decryptResult struct {
	match     string
	plaintext string
	err       error
}

// decryptWrapper wraps the decrypt method for concurrency. ctx is the context that will get canceled if one or more
// decryptions fail. This will halt the Goroutine early. submatches is the channel with the incoming strings to decrypt
// which gets closed when all the encrypted values have been decrypted. Its values are string slices with the first
// index being the whole string that will be replaced and second index being the base64 of the encrypted string. results
// is a channel to communicate back to the calling Goroutine.
func (t *TemplateResolver) decryptWrapper(
	ctx context.Context, submatches <-chan []string, results chan<- decryptResult,
) {
	for submatch := range submatches {
		match := submatch[0]
		encryptedValue := submatch[1]
		var result decryptResult

		plaintext, err := t.decrypt(encryptedValue)
		if err != nil {
			result = decryptResult{match, "", err}
		} else {
			result = decryptResult{match, plaintext, nil}
		}

		select {
		case <-ctx.Done():
			// Return when decryption has been canceled.
			return
		case results <- result:
			// Continue on to the next submatch.
			continue
		}
	}
}

// jsonToYAML converts JSON to YAML using yaml.v3. This is important since
// line wrapping is disabled in v3.
func jsonToYAML(j []byte) ([]byte, error) {
	// Convert the JSON to an object
	var jsonObj interface{}

	err := yaml.Unmarshal(j, &jsonObj)
	if err != nil {
		return nil, err // nolint:wrapcheck
	}

	// Marshal this object into YAML
	var b bytes.Buffer
	yamlEncoder := yaml.NewEncoder(&b)
	yamlEncoder.SetIndent(yamlIndentation)

	err = yamlEncoder.Encode(&jsonObj)
	if err != nil {
		return nil, err // nolint:wrapcheck
	}

	return b.Bytes(), nil
}

// yamlToJSON converts YAML to JSON.
func yamlToJSON(y []byte) ([]byte, error) {
	// Convert the YAML to an object.
	var yamlObj interface{}

	err := yaml.Unmarshal(y, &yamlObj)
	if err != nil {
		return nil, err // nolint:wrapcheck
	}

	// Convert this object to JSON
	return json.Marshal(yamlObj) // nolint:wrapcheck
}

func (t *TemplateResolver) indent(spaces int, v string) string {
	pad := strings.Repeat(" ", spaces+int(t.config.AdditionalIndentation))
	npad := "\n" + pad + strings.Replace(v, "\n", "\n"+pad, -1)

	return strings.TrimSpace(npad)
}

// This is so that the user gets a nicer error in the event some valid scenario slips through the
// regex.
func autoindent(v string) (string, error) {
	return "", errors.New("an unexpeceted error occurred where autoindent could not be processed")
}

func toInt(v interface{}) int {
	return cast.ToInt(v)
}

func atoi(a string) int {
	i, _ := strconv.Atoi(a)

	return i
}

func toBool(a string) bool {
	b, _ := strconv.ParseBool(a)

	return b
}
