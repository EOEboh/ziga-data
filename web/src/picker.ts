// Google Picker integration for the "attach an existing sheet" flow. The Picker
// is the drive.file mechanism: the user selects one spreadsheet, which grants
// the app per-file access to exactly that file — no broad Drive scope, no
// server-side Drive listing. The Google client libraries are loaded on demand.

/* eslint-disable @typescript-eslint/no-explicit-any */
declare global {
  interface Window {
    gapi: any;
    google: any;
  }
}

const DRIVE_FILE_SCOPE = "https://www.googleapis.com/auth/drive.file";

function loadScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    if (document.querySelector(`script[src="${src}"]`)) return resolve();
    const el = document.createElement("script");
    el.src = src;
    el.async = true;
    el.onload = () => resolve();
    el.onerror = () => reject(new Error(`failed to load ${src}`));
    document.head.appendChild(el);
  });
}

// requestDriveToken uses Google Identity Services to obtain a short-lived
// drive.file access token for the Picker (client-side; the server keeps its own
// offline tokens from the OAuth sign-in).
function requestDriveToken(clientId: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const client = window.google.accounts.oauth2.initTokenClient({
      client_id: clientId,
      scope: DRIVE_FILE_SCOPE,
      callback: (resp: any) => {
        if (resp.error) reject(new Error(resp.error));
        else resolve(resp.access_token);
      },
    });
    client.requestAccessToken();
  });
}

function loadPickerApi(): Promise<void> {
  return new Promise((resolve, reject) => {
    window.gapi.load("picker", { callback: () => resolve(), onerror: () => reject(new Error("picker load failed")) });
  });
}

// openSheetPicker loads the libraries, obtains a token, shows the Picker limited
// to spreadsheets, and resolves with the chosen spreadsheet id (or null if the
// user cancels).
export async function openSheetPicker(clientId: string, apiKey: string): Promise<string | null> {
  await Promise.all([
    loadScript("https://accounts.google.com/gsi/client"),
    loadScript("https://apis.google.com/js/api.js"),
  ]);
  await loadPickerApi();
  const token = await requestDriveToken(clientId);

  return new Promise((resolve, reject) => {
    try {
      const g = window.google;
      const view = new g.picker.DocsView(g.picker.ViewId.SPREADSHEETS).setMode(g.picker.DocsViewMode.LIST);
      const picker = new g.picker.PickerBuilder()
        .addView(view)
        .setOAuthToken(token)
        .setDeveloperKey(apiKey)
        .setCallback((data: any) => {
          if (data[g.picker.Response.ACTION] === g.picker.Action.PICKED) {
            resolve(data[g.picker.Response.DOCUMENTS][0][g.picker.Document.ID]);
          } else if (data[g.picker.Response.ACTION] === g.picker.Action.CANCEL) {
            resolve(null);
          }
        })
        .build();
      picker.setVisible(true);
    } catch (e) {
      reject(e);
    }
  });
}
