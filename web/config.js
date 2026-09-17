// Deployment-time API endpoint. This file is optional configuration, not a
// secret store: the endpoint can also be set in the console's settings dialog,
// and the bearer token always stays in sessionStorage.
window.AIHARN_CONFIG = {
  endpoint: null
  // endpoint: { name: "Local", url: "http://127.0.0.1:7331" }
  //
  // The former shape is still honoured; when more than one entry is present the
  // first one is used.
  // apis: [{ name: "Local", url: "http://127.0.0.1:7331" }]
};
