// The browser half of a passkey ceremony.
//
// The control plane sends the options with every binary value as base64url,
// and navigator.credentials wants ArrayBuffers, so this converts both ways.
// The public key is read with getPublicKey(), which hands it over in a form
// the gateway reads without parsing CBOR.

export function passkeysSupported() {
  return Boolean(window.PublicKeyCredential && navigator.credentials);
}

function fromB64(s) {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const bin = atob((s + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

function toB64(buf) {
  if (!buf) return "";
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** createPasskey runs navigator.credentials.create and returns what
 *  /auth/passkey/setup and /v1/passkeys take. */
export async function createPasskey(options) {
  const publicKey = {
    ...options,
    challenge: fromB64(options.challenge),
    user: { ...options.user, id: fromB64(options.user.id) },
    excludeCredentials: (options.excludeCredentials || []).map((c) => ({
      ...c,
      id: fromB64(c.id),
    })),
  };
  const cred = await ceremony(() =>
    navigator.credentials.create({ publicKey }),
  );
  const res = cred.response;
  if (!res.getPublicKey || !res.getAuthenticatorData) {
    throw new Error(
      "This browser is too old for passkeys here. Update it, or use another one.",
    );
  }
  return {
    id: toB64(cred.rawId),
    client_data_json: toB64(res.clientDataJSON),
    authenticator_data: toB64(res.getAuthenticatorData()),
    public_key: toB64(res.getPublicKey()),
    algorithm: res.getPublicKeyAlgorithm(),
  };
}

/** getPasskey runs navigator.credentials.get and returns what
 *  /auth/passkey/sign-in takes. */
export async function getPasskey(options) {
  const publicKey = {
    ...options,
    challenge: fromB64(options.challenge),
    allowCredentials: (options.allowCredentials || []).map((c) => ({
      ...c,
      id: fromB64(c.id),
    })),
  };
  const cred = await ceremony(() => navigator.credentials.get({ publicKey }));
  const res = cred.response;
  return {
    id: toB64(cred.rawId),
    client_data_json: toB64(res.clientDataJSON),
    authenticator_data: toB64(res.authenticatorData),
    signature: toB64(res.signature),
    user_handle: toB64(res.userHandle),
  };
}

/** ceremony turns the browser's errors into sentences. */
async function ceremony(run) {
  if (!passkeysSupported()) {
    throw new Error("This browser does not support passkeys.");
  }
  try {
    const cred = await run();
    if (!cred) throw new Error("No passkey was chosen.");
    return cred;
  } catch (err) {
    switch (err && err.name) {
      case "NotAllowedError":
        throw new Error("The passkey prompt was closed or timed out.");
      case "InvalidStateError":
        throw new Error("This device already has a passkey for this account.");
      case "SecurityError":
        throw new Error(
          "The browser refused: open the panel at the address in KEERA_PUBLIC_URL.",
        );
    }
    throw err;
  }
}

/** deviceName guesses a name for a new passkey, which the person can change. */
export function deviceName() {
  const ua = navigator.userAgent || "";
  for (const [needle, name] of [
    ["iPhone", "iPhone"],
    ["iPad", "iPad"],
    ["Android", "Android"],
    ["Mac", "Mac"],
    ["Windows", "Windows"],
    ["CrOS", "Chromebook"],
    ["Linux", "Linux"],
  ]) {
    if (ua.includes(needle)) return name;
  }
  return "Passkey";
}
