// Alt-click on a wire deletes it. The handler used to sit on the wire's click
// event, which never fires because the canvas takes pointer capture on the
// press, so the click went to the canvas and the wire stayed. The count of
// wires before and after is what proves it; the draft is deployed and read back
// from the runtime to prove the wire is gone from the file too.

import { expect, test } from '@playwright/test';
import { canvasNode, stableCentre, deployed, focusCanvas, seed, signIn, tabState, threeNodes, topDeploy } from './helpers';

const wires = (page: import('@playwright/test').Page) => page.locator('.canvas-host > .canvas path.wire');
const wireEl = (page: import('@playwright/test').Page, from: string, to: string) =>
  page.locator(`.canvas-host > .canvas path.wire[data-from="${from}"][data-to="${to}"]`);

test('alt-click deletes a wire, a plain click does not, and undo puts it back', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);
  await expect(wires(page)).toHaveCount(2);

  const at = await stableCentre(page, wireEl(page, 'i1', 'c1'));
  await page.mouse.click(at.x, at.y);
  await expect(wires(page)).toHaveCount(2);

  await page.keyboard.down('Alt');
  await page.mouse.click(at.x, at.y);
  await page.keyboard.up('Alt');
  await expect(wires(page)).toHaveCount(1);
  await expect(wireEl(page, 'i1', 'c1')).toHaveCount(0);
  await expect(wireEl(page, 'c1', 'd1')).toHaveCount(1);
  await expect(canvasNode(page, 'i1')).toBeVisible();

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  const flows = await deployed(request);
  expect(flows.find((e) => e.id === 'i1')!.wires).toEqual([[]]);
  expect(flows.find((e) => e.id === 'c1')!.wires).toEqual([['d1']]);

  await focusCanvas(page);
  await page.keyboard.press('Control+Z');
  await expect(wires(page)).toHaveCount(2);
});
