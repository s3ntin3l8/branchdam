import { describe, expect, it } from "vitest";
import { otpauthSecret } from "./otpauth";

describe("otpauthSecret", () => {
  it("extracts the secret and groups it in fours", () => {
    expect(
      otpauthSecret("otpauth://totp/branchDAM:bob?secret=JBSWY3DPEHPK3PXP&issuer=branchDAM&digits=6"),
    ).toBe("JBSW Y3DP EHPK 3PXP");
  });

  it("handles a secret that is the last parameter and not a multiple of four", () => {
    expect(otpauthSecret("otpauth://totp/x?issuer=a&secret=ABCDEF")).toBe("ABCD EF");
  });

  it("returns an empty string without a secret", () => {
    expect(otpauthSecret("otpauth://totp/x?issuer=a")).toBe("");
  });
});
