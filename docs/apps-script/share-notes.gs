/**
 * Shares every "Notes by Gemini" document you own with the sync service account,
 * wherever Google filed it. This keeps gemini-notes-sync working when Google
 * creates a new "Google Meet" / "Meet Recordings" folder that nobody has shared.
 *
 * It runs as you, inside your Google account. The service account only ever gets
 * Viewer access to the individual note documents, not to folders or anything else.
 *
 * Setup (once):
 *   1. script.google.com -> New project -> paste this file.
 *   2. Left sidebar: Services (+) -> add "Drive API" (v3).
 *   3. Set SERVICE_ACCOUNT below to the e-mail in your key.json ("client_email").
 *   4. Run installTrigger() once and approve the permissions.
 *      (Run shareNewNotes() once first if you want to see the log.)
 */

const SERVICE_ACCOUNT = 'your-service-account@your-project.iam.gserviceaccount.com';
const NAME_CONTAINS = 'Notes by Gemini';

function shareNewNotes() {
  if (typeof Drive === 'undefined') {
    throw new Error('Enable the Drive API service first: Services (+) in the left sidebar -> ' +
      'Drive API -> version v3, identifier "Drive" -> Add.');
  }
  if (SERVICE_ACCOUNT.indexOf('your-service-account') === 0) {
    throw new Error('Set SERVICE_ACCOUNT at the top of the file to the client_email from key.json.');
  }
  const started = Date.now();
  const query = "mimeType = 'application/vnd.google-apps.document'" +
    " and title contains '" + NAME_CONTAINS + "'" +
    " and 'me' in owners and trashed = false";
  const files = DriveApp.searchFiles(query);
  let shared = 0, already = 0;

  while (files.hasNext()) {
    if (Date.now() - started > 4.5 * 60 * 1000) {   // stay under Apps Script's 6 minute limit
      console.log('Out of time; the next run continues.');
      break;
    }
    const file = files.next();
    if (hasViewer(file)) {
      already++;
      continue;
    }
    // sendNotificationEmail:false avoids a bounce e-mail from the robot address.
    Drive.Permissions.create(
      { type: 'user', role: 'reader', emailAddress: SERVICE_ACCOUNT },
      file.getId(),
      { sendNotificationEmail: false });
    shared++;
    console.log('Shared: ' + file.getName());
  }
  console.log('Done. newly shared: ' + shared + ', already shared: ' + already);
}

function hasViewer(file) {
  const target = SERVICE_ACCOUNT.toLowerCase();
  return file.getViewers().concat(file.getEditors())
    .some(function (u) { return u.getEmail().toLowerCase() === target; });
}

/** Runs shareNewNotes every 10 minutes. Run this once. */
function installTrigger() {
  ScriptApp.getProjectTriggers()
    .filter(function (t) { return t.getHandlerFunction() === 'shareNewNotes'; })
    .forEach(function (t) { ScriptApp.deleteTrigger(t); });
  ScriptApp.newTrigger('shareNewNotes').timeBased().everyMinutes(10).create();
}
