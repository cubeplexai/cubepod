package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// UpsertPodBox creates or updates a PodBox Custom Resource in the Kubernetes APIServer.
// It issues a POST to /apis/cubepod.io/v1alpha1/namespaces/{ns}/podboxes.
// If the APIServer returns 409 Conflict (AlreadyExists), it retrieves the existing object's
// metadata.resourceVersion, then issues a PUT to /apis/cubepod.io/v1alpha1/namespaces/{ns}/podboxes/{name}
// and a PUT to .../status.
// If the APIServer returns 404 NotFound because the CRD is not installed, it gracefully logs and returns nil.
func (c *RESTKubeClient) UpsertPodBox(ctx context.Context, box v1alpha1.PodBox) error {
	ns := c.resolveNamespace(box.Metadata.Namespace)
	box.Metadata.Namespace = ns
	name := box.Metadata.Name
	if name == "" {
		return fmt.Errorf("PodBox metadata.name is required")
	}
	if box.APIVersion == "" {
		box.APIVersion = "cubepod.io/v1alpha1"
	}
	if box.Kind == "" {
		box.Kind = "PodBox"
	}

	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podboxes", c.baseURL, ns)
	payload, err := json.Marshal(box)
	if err != nil {
		return fmt.Errorf("failed to marshal PodBox JSON: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("UpsertPodBox POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		slog.Debug("CRD cubepod.io/v1alpha1 not installed on APIServer; skipping PodBox persistence",
			"namespace", ns, "name", name)
		return nil
	}

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	if resp.StatusCode == http.StatusConflict {
		// 409 Conflict: GET existing metadata.resourceVersion, then PUT object and PUT .../status
		existing, err := c.GetPodBox(ctx, ns, name)
		if err != nil {
			return fmt.Errorf("failed to get existing PodBox on conflict: %w", err)
		}

		box.Metadata.ResourceVersion = existing.Metadata.ResourceVersion

		putURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podboxes/%s", c.baseURL, ns, name)
		putPayload, err := json.Marshal(box)
		if err != nil {
			return fmt.Errorf("failed to marshal PodBox JSON for update: %w", err)
		}

		putResp, err := c.doRequest(ctx, http.MethodPut, putURL, bytes.NewReader(putPayload))
		if err != nil {
			return fmt.Errorf("UpsertPodBox PUT failed: %w", err)
		}
		defer putResp.Body.Close()

		if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated {
			putBody, _ := io.ReadAll(putResp.Body)
			return fmt.Errorf("UpsertPodBox PUT failed: status %d (%s)", putResp.StatusCode, string(putBody))
		}

		// Read updated resourceVersion from PUT response if present
		var updatedBox v1alpha1.PodBox
		if err := json.NewDecoder(putResp.Body).Decode(&updatedBox); err == nil && updatedBox.Metadata.ResourceVersion != "" {
			box.Metadata.ResourceVersion = updatedBox.Metadata.ResourceVersion
		}

		// PUT to .../status
		statusURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podboxes/%s/status", c.baseURL, ns, name)
		statusPayload, err := json.Marshal(box)
		if err != nil {
			return fmt.Errorf("failed to marshal PodBox JSON for status update: %w", err)
		}

		statusResp, err := c.doRequest(ctx, http.MethodPut, statusURL, bytes.NewReader(statusPayload))
		if err != nil {
			return fmt.Errorf("UpsertPodBox PUT status failed: %w", err)
		}
		defer statusResp.Body.Close()

		if statusResp.StatusCode != http.StatusOK && statusResp.StatusCode != http.StatusCreated {
			statusBody, _ := io.ReadAll(statusResp.Body)
			return fmt.Errorf("UpsertPodBox PUT status failed: status %d (%s)", statusResp.StatusCode, string(statusBody))
		}

		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("UpsertPodBox failed: status %d (%s)", resp.StatusCode, string(body))
}

// GetPodBox retrieves a PodBox Custom Resource by namespace and name.
func (c *RESTKubeClient) GetPodBox(ctx context.Context, namespace, name string) (*v1alpha1.PodBox, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podboxes/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("GetPodBox request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("podbox %s/%s not found", ns, name)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetPodBox failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var box v1alpha1.PodBox
	if err := json.NewDecoder(resp.Body).Decode(&box); err != nil {
		return nil, fmt.Errorf("failed to decode PodBox: %w", err)
	}
	return &box, nil
}

// ListPodBoxes lists all PodBox Custom Resources in the given namespace.
func (c *RESTKubeClient) ListPodBoxes(ctx context.Context, namespace string) ([]v1alpha1.PodBox, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podboxes", c.baseURL, ns)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ListPodBoxes request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []v1alpha1.PodBox{}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ListPodBoxes failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var list v1alpha1.PodBoxList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("failed to decode PodBoxList: %w", err)
	}
	if list.Items == nil {
		return []v1alpha1.PodBox{}, nil
	}
	return list.Items, nil
}

// DeletePodBox deletes a PodBox Custom Resource by namespace and name, ignoring 404 NotFound.
func (c *RESTKubeClient) DeletePodBox(ctx context.Context, namespace, name string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podboxes/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("DeletePodBox request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted ||
		resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("DeletePodBox failed: status %d (%s)", resp.StatusCode, string(body))
}

// UpsertPodWarmPool creates or updates a PodWarmPool Custom Resource in the Kubernetes APIServer.
func (c *RESTKubeClient) UpsertPodWarmPool(ctx context.Context, pool v1alpha1.PodWarmPool) error {
	ns := c.resolveNamespace(pool.Metadata.Namespace)
	pool.Metadata.Namespace = ns
	name := pool.Metadata.Name
	if name == "" {
		return fmt.Errorf("PodWarmPool metadata.name is required")
	}
	if pool.APIVersion == "" {
		pool.APIVersion = "cubepod.io/v1alpha1"
	}
	if pool.Kind == "" {
		pool.Kind = "PodWarmPool"
	}

	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools", c.baseURL, ns)
	payload, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("failed to marshal PodWarmPool JSON: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("UpsertPodWarmPool POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		slog.Debug("CRD podwarmpools.cubepod.io not installed on APIServer; skipping PodWarmPool persistence",
			"namespace", ns, "name", name)
		return ErrCRDNotInstalled
	}

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var created v1alpha1.PodWarmPool
		if err := json.NewDecoder(resp.Body).Decode(&created); err == nil && created.Metadata.ResourceVersion != "" {
			pool.Metadata.ResourceVersion = created.Metadata.ResourceVersion
		}

		statusURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s/status", c.baseURL, ns, name)
		statusPayload, err := json.Marshal(pool)
		if err != nil {
			return fmt.Errorf("failed to marshal PodWarmPool status JSON: %w", err)
		}
		statusResp, err := c.doRequest(ctx, http.MethodPut, statusURL, bytes.NewReader(statusPayload))
		if err != nil {
			return fmt.Errorf("UpsertPodWarmPool PUT status failed: %w", err)
		}
		defer statusResp.Body.Close()

		if statusResp.StatusCode >= 400 {
			body, _ := io.ReadAll(statusResp.Body)
			return fmt.Errorf("UpsertPodWarmPool PUT status failed: status %d (%s)", statusResp.StatusCode, string(body))
		}
		return nil
	}

	if resp.StatusCode == http.StatusConflict {
		existing, err := c.GetPodWarmPool(ctx, ns, name)
		if err != nil {
			return fmt.Errorf("UpsertPodWarmPool conflict resolution failed to GET existing object: %w", err)
		}
		pool.Metadata.ResourceVersion = existing.Metadata.ResourceVersion

		putURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s", c.baseURL, ns, name)
		putPayload, err := json.Marshal(pool)
		if err != nil {
			return fmt.Errorf("failed to marshal PodWarmPool for PUT: %w", err)
		}
		putResp, err := c.doRequest(ctx, http.MethodPut, putURL, bytes.NewReader(putPayload))
		if err != nil {
			return fmt.Errorf("UpsertPodWarmPool PUT failed: %w", err)
		}
		defer putResp.Body.Close()

		if putResp.StatusCode >= 400 {
			body, _ := io.ReadAll(putResp.Body)
			return fmt.Errorf("UpsertPodWarmPool PUT failed: status %d (%s)", putResp.StatusCode, string(body))
		}

		var updated v1alpha1.PodWarmPool
		if err := json.NewDecoder(putResp.Body).Decode(&updated); err == nil && updated.Metadata.ResourceVersion != "" {
			pool.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		}

		statusURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s/status", c.baseURL, ns, name)
		statusPayload, err := json.Marshal(pool)
		if err != nil {
			return fmt.Errorf("failed to marshal PodWarmPool for status update: %w", err)
		}
		statusResp, err := c.doRequest(ctx, http.MethodPut, statusURL, bytes.NewReader(statusPayload))
		if err != nil {
			return fmt.Errorf("UpsertPodWarmPool PUT status failed: %w", err)
		}
		defer statusResp.Body.Close()

		if statusResp.StatusCode >= 400 {
			body, _ := io.ReadAll(statusResp.Body)
			return fmt.Errorf("UpsertPodWarmPool PUT status failed: status %d (%s)", statusResp.StatusCode, string(body))
		}
		return nil
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("UpsertPodWarmPool POST failed: status %d (%s)", resp.StatusCode, string(body))
	}

	return nil
}

// UpdatePodWarmPoolStatus updates only the status subresource of a PodWarmPool Custom Resource in the Kubernetes APIServer.
func (c *RESTKubeClient) UpdatePodWarmPoolStatus(ctx context.Context, namespace, name string, status v1alpha1.PodWarmPoolStatus) error {
	ns := c.resolveNamespace(namespace)
	existing, err := c.GetPodWarmPool(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("UpdatePodWarmPoolStatus failed to get existing PodWarmPool: %w", err)
	}

	existing.Status = status
	statusURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s/status", c.baseURL, ns, name)
	statusPayload, err := json.Marshal(existing)
	if err != nil {
		return fmt.Errorf("failed to marshal PodWarmPool for status update: %w", err)
	}

	statusResp, err := c.doRequest(ctx, http.MethodPut, statusURL, bytes.NewReader(statusPayload))
	if err != nil {
		return fmt.Errorf("UpdatePodWarmPoolStatus PUT status failed: %w", err)
	}
	defer statusResp.Body.Close()

	if statusResp.StatusCode >= 400 {
		body, _ := io.ReadAll(statusResp.Body)
		return fmt.Errorf("UpdatePodWarmPoolStatus PUT status failed: status %d (%s)", statusResp.StatusCode, string(body))
	}

	return nil
}

// GetPodWarmPool retrieves a single PodWarmPool Custom Resource by namespace and name.
func (c *RESTKubeClient) GetPodWarmPool(ctx context.Context, namespace, name string) (*v1alpha1.PodWarmPool, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("GetPodWarmPool GET failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("PodWarmPool %s/%s not found (404)", ns, name)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetPodWarmPool failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var pool v1alpha1.PodWarmPool
	if err := json.NewDecoder(resp.Body).Decode(&pool); err != nil {
		return nil, fmt.Errorf("failed to decode PodWarmPool: %w", err)
	}
	return &pool, nil
}

// ListPodWarmPools retrieves all PodWarmPool Custom Resources in the given namespace.
func (c *RESTKubeClient) ListPodWarmPools(ctx context.Context, namespace string) ([]v1alpha1.PodWarmPool, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools", c.baseURL, ns)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ListPodWarmPools GET failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrCRDNotInstalled
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ListPodWarmPools failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var list v1alpha1.PodWarmPoolList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("failed to decode PodWarmPoolList: %w", err)
	}
	return list.Items, nil
}

// DeletePodWarmPool removes a PodWarmPool Custom Resource by namespace and name.
func (c *RESTKubeClient) DeletePodWarmPool(ctx context.Context, namespace, name string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podwarmpools/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("DeletePodWarmPool DELETE failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted ||
		resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("DeletePodWarmPool failed: status %d (%s)", resp.StatusCode, string(body))
}

// ListPodTemplates lists all PodTemplate Custom Resources in the given namespace.
func (c *RESTKubeClient) ListPodTemplates(ctx context.Context, namespace string) ([]v1alpha1.PodTemplate, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podtemplates", c.baseURL, ns)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ListPodTemplates request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []v1alpha1.PodTemplate{}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ListPodTemplates failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var list v1alpha1.PodTemplateList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("failed to decode PodTemplateList: %w", err)
	}
	if list.Items == nil {
		return []v1alpha1.PodTemplate{}, nil
	}
	return list.Items, nil
}

// GetPodTemplate retrieves a PodTemplate Custom Resource by namespace and name.
func (c *RESTKubeClient) GetPodTemplate(ctx context.Context, namespace, name string) (*v1alpha1.PodTemplate, error) {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podtemplates/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("GetPodTemplate request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("podtemplate %s/%s not found", ns, name)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetPodTemplate failed: status %d (%s)", resp.StatusCode, string(body))
	}

	var tpl v1alpha1.PodTemplate
	if err := json.NewDecoder(resp.Body).Decode(&tpl); err != nil {
		return nil, fmt.Errorf("failed to decode PodTemplate: %w", err)
	}
	return &tpl, nil
}

// CreatePodTemplate creates a PodTemplate Custom Resource in the Kubernetes APIServer.
func (c *RESTKubeClient) CreatePodTemplate(ctx context.Context, template v1alpha1.PodTemplate) error {
	ns := c.resolveNamespace(template.Metadata.Namespace)
	template.Metadata.Namespace = ns
	name := template.Metadata.Name
	if name == "" {
		return fmt.Errorf("PodTemplate metadata.name is required")
	}
	if template.APIVersion == "" {
		template.APIVersion = "cubepod.io/v1alpha1"
	}
	if template.Kind == "" {
		template.Kind = "PodTemplate"
	}

	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podtemplates", c.baseURL, ns)
	payload, err := json.Marshal(template)
	if err != nil {
		return fmt.Errorf("failed to marshal PodTemplate JSON: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("CreatePodTemplate POST failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("podtemplate %s/%s already exists: status 409", ns, name)
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("CreatePodTemplate failed: status %d (%s)", resp.StatusCode, string(body))
}

// UpdatePodTemplateStatus updates the status subresource of a PodTemplate Custom Resource.
func (c *RESTKubeClient) UpdatePodTemplateStatus(ctx context.Context, template v1alpha1.PodTemplate) error {
	ns := c.resolveNamespace(template.Metadata.Namespace)
	name := template.Metadata.Name
	if name == "" {
		return fmt.Errorf("PodTemplate metadata.name is required")
	}

	existing, err := c.GetPodTemplate(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("failed to get existing PodTemplate for status update: %w", err)
	}
	template.Metadata.ResourceVersion = existing.Metadata.ResourceVersion
	if template.APIVersion == "" {
		template.APIVersion = "cubepod.io/v1alpha1"
	}
	if template.Kind == "" {
		template.Kind = "PodTemplate"
	}

	statusURL := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podtemplates/%s/status", c.baseURL, ns, name)
	payload, err := json.Marshal(template)
	if err != nil {
		return fmt.Errorf("failed to marshal PodTemplate status JSON: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPut, statusURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("UpdatePodTemplateStatus PUT failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("UpdatePodTemplateStatus failed: status %d (%s)", resp.StatusCode, string(body))
}

// DeletePodTemplate deletes a PodTemplate Custom Resource by namespace and name, ignoring 404 NotFound.
func (c *RESTKubeClient) DeletePodTemplate(ctx context.Context, namespace, name string) error {
	ns := c.resolveNamespace(namespace)
	url := fmt.Sprintf("%s/apis/cubepod.io/v1alpha1/namespaces/%s/podtemplates/%s", c.baseURL, ns, name)

	resp, err := c.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("DeletePodTemplate request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted ||
		resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("DeletePodTemplate failed: status %d (%s)", resp.StatusCode, string(body))
}
