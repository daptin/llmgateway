package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestInvokeImageEditPreservesMultipleImagesAndMask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/images/edits" || !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Fatalf("unexpected upstream image edit request: path=%s content-type=%q", request.URL.Path, request.Header.Get("Content-Type"))
		}
		if err := request.ParseMultipartForm(4096); err != nil {
			t.Fatal(err)
		}
		images := request.MultipartForm.File["image[]"]
		masks := request.MultipartForm.File["mask"]
		if len(images) != 2 || len(masks) != 1 || images[0].Filename != "one.png" || masks[0].Filename != "mask.png" ||
			request.FormValue("model") != "upstream-model" || request.FormValue("prompt") != "remove background" ||
			request.FormValue("output_compression") != "80" || request.FormValue("vendor_flag") != "enabled" {
			t.Fatalf("unexpected image edit form: values=%v files=%v", request.MultipartForm.Value, request.MultipartForm.File)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"created":100,"data":[{"b64_json":"edited"}]}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	compression := 80
	result, err := adapter.Invoke(context.Background(), deploymentWithParameters(`{"image_edit":{"vendor_flag":"enabled"}}`), contract.Request{
		Operation: contract.OperationImageEdit, ImageEdit: &contract.ImageEditRequest{
			Images: []contract.MediaFile{{Name: "one.png", Data: []byte("one")}, {Name: "two.png", Data: []byte("two")}},
			Mask:   &contract.MediaFile{Name: "mask.png", Data: []byte("mask")}, Prompt: "remove background", N: 1,
			ResponseFormat: "b64_json", OutputCompression: &compression,
		}})
	if err != nil || result.Images == nil || len(result.Images.Data) != 1 || result.Images.Data[0].Base64 != "edited" {
		t.Fatalf("image edit result=%#v err=%v", result, err)
	}
}

func TestInvokeImageVariationUsesCanonicalMultipartRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/images/variations" || !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Fatalf("unexpected upstream image variation request: path=%s content-type=%q", request.URL.Path, request.Header.Get("Content-Type"))
		}
		if err := request.ParseMultipartForm(4096); err != nil {
			t.Fatal(err)
		}
		image := request.MultipartForm.File["image"]
		if len(image) != 1 || image[0].Filename != "source.png" || request.FormValue("model") != "upstream-model" ||
			request.FormValue("n") != "1" || request.FormValue("size") != "512x512" ||
			request.FormValue("response_format") != "b64_json" || request.FormValue("vendor_flag") != "enabled" {
			t.Fatalf("unexpected image variation form: values=%v files=%v", request.MultipartForm.Value, request.MultipartForm.File)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"created":101,"data":[{"b64_json":"variation"}]}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	result, err := adapter.Invoke(context.Background(), deploymentWithParameters(`{"image_variation":{"vendor_flag":"enabled"}}`), contract.Request{
		Operation: contract.OperationImageVariation, ImageVariation: &contract.ImageVariationRequest{
			Image: contract.MediaFile{Name: "source.png", Data: []byte("image")}, N: 1, Size: "512x512", ResponseFormat: "b64_json",
		}})
	if err != nil || result.Images == nil || len(result.Images.Data) != 1 || result.Images.Data[0].Base64 != "variation" {
		t.Fatalf("image variation result=%#v err=%v", result, err)
	}
}
