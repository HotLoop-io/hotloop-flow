// Two-factor sign-in from the editor, start to finish, against the real binary:
// set it up by scanning (well, reading) the key, sign in with a code, turn it
// off with a code. The codes are worked out here with RFC 6238 from the key the
// editor showed, exactly the way an authenticator app does it.

import { createHmac } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';

const user = 'kim';
const password = 'e2e-kim-password';

function base32(s: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const ch of s.replace(/\s+/g, '').toUpperCase()) {
    const v = alphabet.indexOf(ch);
    if (v < 0) throw new Error(`not base32: ${ch}`);
    bits += v.toString(2).padStart(5, '0');
  }
  const bytes: number[] = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(bytes);
}

/** The code for a time, offset in thirty-second steps from now. */
function totp(secret: string, stepsFromNow = 0): string {
  const step = Math.floor(Date.now() / 30000) + stepsFromNow;
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(step));
  const sum = createHmac('sha1', base32(secret)).update(msg).digest();
  const off = sum[sum.length - 1]! & 0x0f;
  const bin = ((sum[off]! & 0x7f) << 24) | (sum[off + 1]! << 16) | (sum[off + 2]! << 8) | sum[off + 3]!;
  return String(bin % 1_000_000).padStart(6, '0');
}

async function signIn(page: Page, code?: string): Promise<void> {
  await page.goto('/');
  await page.locator('#u').fill(user);
  await page.locator('#p').fill(password);
  await page.locator('form button[type="submit"]').click();
  if (code !== undefined) {
    await expect(page.locator('#c')).toBeVisible();
    await expect(page.locator('.login .error')).toContainText('six-digit code');
    await page.locator('#c').fill(code);
    await page.locator('form button[type="submit"]').click();
  }
  await expect(page.locator('[data-test="mfa-open"]')).toBeVisible();
}

test('two-factor sign-in: set it up, sign in with it, turn it off', async ({ page, request }) => {
  // Up to thirty seconds of it is waiting for the next code.
  test.setTimeout(120_000);
  await signIn(page);

  // Set up: the dialog shows a QR code and the key, and a wrong code is asked
  // for again rather than starting over.
  await page.locator('[data-test="mfa-open"]').click();
  await page.locator('[data-test="mfa-start"]').click();
  await expect(page.locator('.dialog svg.qr path')).toHaveCount(1);
  const secret = (await page.locator('[data-test="mfa-secret"]').textContent())!.replace(/\s+/g, '');
  expect(secret).toMatch(/^[A-Z2-7]{32}$/);

  const wrong = totp(secret) === '000000' ? '111111' : '000000';
  await page.locator('[data-test="mfa-confirm-code"]').fill(wrong);
  await page.locator('[data-test="mfa-confirm"]').click();
  await expect(page.locator('.dialog .dialog-error')).toContainText('wrong');
  await expect(page.locator('[data-test="mfa-secret"]')).toHaveText(/./);
  await page.locator('[data-test="mfa-confirm-code"]').fill(totp(secret));
  await page.locator('[data-test="mfa-confirm"]').click();
  await expect(page.locator('.dialog h2')).toHaveText('Two-factor sign-in is on');
  await page.locator('[data-test="mfa-done"]').click();

  // The password alone no longer gets anybody in, over the API either.
  const bare = await request.post('/auth/token', { data: { username: user, password } });
  expect(bare.status()).toBe(401);
  expect((await bare.json()).mfa).toBe('required');

  // Sign out and back in with a code. One step ahead, because the code that
  // confirmed the setup is spent, and the runtime allows one step of skew.
  await page.locator('.topbar button', { hasText: 'Sign out' }).click();
  await signIn(page, totp(secret, 1));

  // Turn it off, which takes a code too.
  await page.locator('[data-test="mfa-open"]').click();
  await page.locator('[data-test="mfa-disable-code"]').fill(totp(secret, -1));
  await page.locator('[data-test="mfa-disable"]').click();
  await expect(page.locator('.dialog .dialog-error')).toContainText('wrong');
  // A spent step isn't accepted, so a fresh code means waiting for the clock.
  await page.waitForTimeout(30_000 - (Date.now() % 30_000) + 500);
  await page.locator('[data-test="mfa-disable-code"]').fill(totp(secret, 1));
  await page.locator('[data-test="mfa-disable"]').click();
  await expect(page.locator('.dialog')).toHaveCount(0);

  const after = await request.post('/auth/token', { data: { username: user, password } });
  expect(after.status()).toBe(200);
});
