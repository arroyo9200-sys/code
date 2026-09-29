package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
)

var (
	driveToken   string
	driveTokenMu sync.Mutex
	oauthState   string
)

func driveConfigured() bool {
	return os.Getenv("GOOGLE_CLIENT_ID") != "" && os.Getenv("GOOGLE_CLIENT_SECRET") != ""
}

func driveRedirectURI() string {
	return "https://3000-" + os.Getenv("BASE44_PUBLIC_HOST_SUFFIX") + "/api/drive/callback"
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// handleDriveAuth redirects the user to Google's OAuth consent screen.
func handleDriveAuth(w http.ResponseWriter, r *http.Request) {
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	if clientID == "" {
		http.Error(w, "Google OAuth not configured. Set GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET.", http.StatusServiceUnavailable)
		return
	}
	oauthState = randomHex(16)
	params := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {driveRedirectURI()},
		"response_type": {"code"},
		"scope":         {"https://www.googleapis.com/auth/drive.file"},
		"state":         {oauthState},
		"prompt":        {"consent"},
	}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+params.Encode(), http.StatusFound)
}

// handleDriveCallback exchanges the authorization code for an access token.
func handleDriveCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" || state != oauthState {
		http.Error(w, "Invalid OAuth callback", http.StatusBadRequest)
		return
	}
	resp, err := http.PostForm("https://oauth2.googleapis.com/token", url.Values{
		"client_id":     {os.Getenv("GOOGLE_CLIENT_ID")},
		"client_secret": {os.Getenv("GOOGLE_CLIENT_SECRET")},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {driveRedirectURI()},
	})
	if err != nil {
		http.Error(w, "Token exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	if tok.AccessToken == "" {
		http.Error(w, "No access token received from Google", http.StatusInternalServerError)
		return
	}
	driveTokenMu.Lock()
	driveToken = tok.AccessToken
	driveTokenMu.Unlock()
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDriveStatus reports whether Google Drive is configured and authenticated.
func handleDriveStatus(w http.ResponseWriter, r *http.Request) {
	driveTokenMu.Lock()
	authed := driveToken != ""
	driveTokenMu.Unlock()
	writeJSON(w, map[string]bool{
		"authenticated": authed,
		"configured":    driveConfigured(),
	})
}

// handleDriveSave uploads the provided source code as a new file to Google Drive.
func handleDriveSave(w http.ResponseWriter, r *http.Request) {
	driveTokenMu.Lock()
	token := driveToken
	driveTokenMu.Unlock()
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "Not authenticated. Click Save to Drive to connect your Google account."})
		return
	}

	var req struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "Invalid request body"})
		return
	}
	if req.Filename == "" || req.Content == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "filename and content are required"})
		return
	}

	// Build a multipart/related body: metadata part + content part.
	metadata, _ := json.Marshal(map[string]string{"name": req.Filename})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	metaPart, _ := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type": []string{"application/json; charset=UTF-8"},
	})
	metaPart.Write(metadata)

	contentPart, _ := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type": []string{"text/plain; charset=UTF-8"},
	})
	contentPart.Write([]byte(req.Content))
	writer.Close()

	uploadReq, _ := http.NewRequest("POST",
		"https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart", &body)
	uploadReq.Header.Set("Authorization", "Bearer "+token)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(uploadReq)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Upload failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		// Token may be expired — clear it so the user can re-auth.
		driveTokenMu.Lock()
		driveToken = ""
		driveTokenMu.Unlock()
		w.WriteHeader(resp.StatusCode)
		writeJSON(w, map[string]string{"error": "Drive API error (" + resp.Status + "): " + strings.TrimSpace(string(respBody))})
		return
	}

	writeJSON(w, map[string]any{"ok": true, "filename": req.Filename})
}
