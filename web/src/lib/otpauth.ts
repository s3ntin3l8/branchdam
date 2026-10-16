// otpauthSecret pulls the base32 secret out of an otpauth:// URI, for
// people who type it into their authenticator instead of scanning the QR
// code. Grouped in fours, the way authenticator apps show it. Returns ""
// when the URI carries no secret.
export function otpauthSecret(uri: string): string {
  const match = /[?&]secret=([^&]+)/.exec(uri);
  if (!match) return "";
  return decodeURIComponent(match[1]).replace(/(.{4})(?=.)/g, "$1 ");
}
