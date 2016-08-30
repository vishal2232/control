package ui

import (
	"net/http"

	"github.com/supergiant/supergiant/pkg/client"
	"github.com/supergiant/supergiant/pkg/models"
)

func NewCloudAccount(sg *client.Client, w http.ResponseWriter, r *http.Request) error {
	return renderTemplate(w, "cloud_accounts/new.html", map[string]interface{}{
		"title":      "Cloud Accounts",
		"formAction": "/ui/cloud_accounts",
		"model": map[string]interface{}{
			"name":     "",
			"provider": "",
			"credentials": map[string]interface{}{
				"access_key": "",
				"secret_key": "",
			},
		},
	})
}

func CreateCloudAccount(sg *client.Client, w http.ResponseWriter, r *http.Request) error {
	m := new(models.CloudAccount)
	if err := unmarshalFormInto(r, m); err != nil {
		return err
	}
	if err := sg.CloudAccounts.Create(m); err != nil {
		return renderTemplate(w, "cloud_accounts/new.html", map[string]interface{}{
			"title":      "Cloud Accounts",
			"formAction": "/ui/cloud_accounts",
			"model":      m,
			"error":      err.Error(),
		})
	}
	http.Redirect(w, r, "/ui/cloud_accounts", http.StatusFound)
	return nil
}

func ListCloudAccounts(sg *client.Client, w http.ResponseWriter, r *http.Request) error {
	fields := []map[string]interface{}{
		{
			"title": "Name",
			"type":  "field_value",
			"field": "name",
		},
		{
			"title": "Provider",
			"type":  "field_value",
			"field": "provider",
		},
	}
	return renderTemplate(w, "cloud_accounts/index.html", map[string]interface{}{
		"title":       "Cloud Accounts",
		"uiBasePath":  "/ui/cloud_accounts",
		"apiListPath": "/api/v0/cloud_accounts",
		"fields":      fields,
		"showNewLink": true,
		"batchActionPaths": map[string]string{
			"Delete": "/delete",
		},
	})
}

func GetCloudAccount(sg *client.Client, w http.ResponseWriter, r *http.Request) error {
	id, err := parseID(r)
	if err != nil {
		return err
	}
	item := new(models.CloudAccount)
	if err := sg.CloudAccounts.Get(id, item); err != nil {
		return err
	}
	return renderTemplate(w, "cloud_accounts/show.html", map[string]interface{}{
		"title": "Cloud Accounts",
		"model": item,
	})
}

func DeleteCloudAccount(sg *client.Client, w http.ResponseWriter, r *http.Request) error {
	id, err := parseID(r)
	if err != nil {
		return err
	}
	item := new(models.CloudAccount)
	item.ID = id
	if err := sg.CloudAccounts.Delete(item); err != nil {
		return err
	}
	// http.Redirect(w, r, "/ui/cloud_accounts", http.StatusFound)
	return nil
}
