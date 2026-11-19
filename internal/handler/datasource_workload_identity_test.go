package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type connectionValidationService struct {
	interfaces.DataSourceService
	calls int
}

func (s *connectionValidationService) ValidateCredentials(context.Context, string, map[string]interface{}) error {
	s.calls++
	return nil
}

func TestConnectionValidationWorkloadIdentityAndExistingConnectors(t *testing.T) {
	for _, tt := range []struct {
		name, body    string
		status, calls int
	}{
		{"managed omitted", `{"type":"document_service"}`, 200, 1},
		{"managed empty", `{"type":"document_service","credentials":{}}`, 200, 1},
		{"managed rejects secret", `{"type":"document_service","credentials":{"token":"secret"}}`, 400, 0},
		{"existing requires map", `{"type":"github"}`, 400, 0},
		{"existing empty map unchanged", `{"type":"github","credentials":{}}`, 200, 1},
		{"existing credentials", `{"type":"github","credentials":{"token":"secret"}}`, 200, 1},
		{"type required", `{}`, 400, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service := &connectionValidationService{}
			h := NewDataSourceHandler(service, nil)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/test", func(c *gin.Context) {
				c.Set(types.TenantIDContextKey.String(), uint64(1))
				h.ValidateCredentials(c)
			})
			req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			require.Equal(t, tt.status, out.Code, out.Body.String())
			require.Equal(t, tt.calls, service.calls)
			require.NotContains(t, out.Body.String(), "secret")
		})
	}
}

func TestManagedCredentialHTTPMutationsAreRejected(t *testing.T) {
	dsService := &stubDataSourceService{
		getDataSource: func(context.Context, string) (*types.DataSource, error) {
			return &types.DataSource{ID: "source", KnowledgeBaseID: "kb", Type: types.ConnectorTypeDocumentService}, nil
		},
	}
	kbService := &stubKBServiceForDS{
		getByID: func(context.Context, string) (*types.KnowledgeBase, error) {
			return &types.KnowledgeBase{ID: "kb", TenantID: 1}, nil
		},
	}
	h := NewDataSourceCredentialsHandler(dsService, kbService)
	router := gin.New()
	router.Use(errorCapture(), func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(1))
	})
	router.PUT("/datasource/:id/credentials", h.Put)
	router.DELETE("/datasource/:id/credentials/:field", h.DeleteField)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			url := "/datasource/source/credentials"
			if method == http.MethodDelete {
				url += "/credentials"
			}
			req := httptest.NewRequest(method, url, strings.NewReader(`{"credentials":{"token":"secret"}}`))
			req.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			require.Equal(t, http.StatusBadRequest, out.Code, out.Body.String())
			require.Contains(t, out.Body.String(), "managed by the deployment")
			require.NotContains(t, out.Body.String(), "secret")
		})
	}
}
