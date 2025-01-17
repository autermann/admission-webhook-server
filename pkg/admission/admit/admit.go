/**
 * Admission mutation handler utility.
 */
package admit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/52north/admission-webhook-server/pkg/utils"
	admissionV1 "k8s.io/api/admission/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Query base path
const (
	ENV_BASE_PATH = "BASE_PATH"
	basePath      = "/mutate"
)

const (
	jsonContentType = `application/json`
)

var (
	UniversalDeserializer = serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
)

// patchOperation is an operation of a JSON patch, see https://tools.ietf.org/html/rfc6902 .
type PatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// admitFunc is a callback for admission controller logic. Given an AdmissionRequest, it returns the sequence of patch
// operations to be applied in case of success, or the error that will be shown when the operation is rejected.
type AdmitFunc func(*admissionV1.AdmissionRequest) ([]PatchOperation, error)

// Get server base path
func GetBasePath() string {
	return utils.GetEnvVal(ENV_BASE_PATH, basePath)
}

// isKubeNamespace checks if the given namespace is a Kubernetes-owned namespace.
func isKubeNamespace(ns string) bool {
	return ns == metaV1.NamespacePublic || ns == metaV1.NamespaceSystem
}

type AdmissionController interface {
	http.Handler
	Register(name string, adm AdmitFunc)
}

type admissionController struct {
	admitFuncs []AdmitFunc
}

func New() AdmissionController {
	return &admissionController{}
}

// Register registers a new AdmitFunc at this controller.
func (ac *admissionController) Register(name string, adm AdmitFunc) {
	log.Printf("registering %s", name)
	ac.admitFuncs = append(ac.admitFuncs, adm)
}

// doServeAdmitFunc parses the HTTP request for an admission controller webhook, and -- in case of a well-formed
// request -- delegates the admission control logic to the given admitFunc. The response body is then returned as raw
// bytes.
func (ac *admissionController) doServeAdmitFunc(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	// Step 1: Request validation. Only handle POST requests with a body and json content type.

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, fmt.Errorf("invalid method %s, only POST requests are allowed", r.Method)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("could not read request body: %v", err)
	}

	if contentType := r.Header.Get("Content-Type"); contentType != jsonContentType {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("unsupported content type %s, only %s is supported", contentType, jsonContentType)
	}

	// Step 2: Parse the AdmissionReview request.

	var admissionReviewReq admissionV1.AdmissionReview

	if _, _, err := UniversalDeserializer.Decode(body, nil, &admissionReviewReq); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, fmt.Errorf("could not deserialize request: %v", err)
	} else if admissionReviewReq.Request == nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, errors.New("malformed admission review: request is nil")
	}

	// Step 3: Construct the AdmissionReview response.

	admissionReviewResponse := &admissionV1.AdmissionReview{
		TypeMeta: admissionReviewReq.TypeMeta,
		Request:  admissionReviewReq.Request,
		Response: &admissionV1.AdmissionResponse{
			UID: admissionReviewReq.Request.UID,
		},
	}

	var patchOps []PatchOperation
	// Apply the admit() function only for non-Kubernetes namespaces. For objects in Kubernetes namespaces, return
	// an empty set of patch operations.
	if !isKubeNamespace(admissionReviewReq.Request.Namespace) {
		for _, adm := range ac.admitFuncs {
			if patches, err := adm(admissionReviewReq.Request); err != nil {
				break
			} else {
				patchOps = append(patchOps, patches...)
			}
		}

	}

	if err != nil {
		// If the handler returned an error, incorporate the error message into the response and deny the object
		// creation.
		admissionReviewResponse.Response.Allowed = false
		admissionReviewResponse.Response.Result = &metaV1.Status{Message: err.Error()}
	} else {
		// Otherwise, encode the patch operations to JSON and return a positive response.
		patchBytes, err := json.Marshal(patchOps)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return nil, fmt.Errorf("could not marshal JSON patch: %v", err)
		}

		admissionReviewResponse.Response.Allowed = true
		admissionReviewResponse.Response.Patch = patchBytes
		patchType := admissionV1.PatchTypeJSONPatch
		admissionReviewResponse.Response.PatchType = &patchType
	}

	// Return the AdmissionReview with a response as JSON.
	bytes, err := json.Marshal(admissionReviewResponse)
	if err != nil {
		return nil, fmt.Errorf("marshaling response: %v", err)
	}

	return bytes, nil
}

// serveAdmitFunc is a wrapper around doServeAdmitFunc that adds error handling and logging.
func (ac *admissionController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	//log.Print("Handling webhook request ...")

	var writeErr error
	if bytes, err := ac.doServeAdmitFunc(w, r); err != nil {
		log.Printf("Error handling webhook request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, writeErr = w.Write([]byte(err.Error()))
	} else {
		_, writeErr = w.Write(bytes)
	}

	if writeErr != nil {
		log.Printf("Could not write response: %v", writeErr)
	}
}
