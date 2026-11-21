// A flow file Node-RED shipped, brought in through the editor's Import dialog by
// its file picker, deployed, run, and judged by what the runtime then says.
// The file is one of Node-RED's own examples (Apache-2.0, see NOTICE).

import { expect, test } from '@playwright/test';
import { allNodes, deployed, press, seed, signIn, tabState, topDeploy, threeNodes } from './helpers';
import { mkdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const example = path.resolve(here, '..', '..', 'testdata', 'nodered', 'examples', 'function__change__01 - Set payload value.json');

mkdirSync('shots', { recursive: true });

test('a Node-RED example imports through the dialog, deploys, and prints what Node-RED prints', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);
  await page.locator('[data-test="import-open"]').click();
  await page.locator('[data-test="import-file"]').setInputFiles(example);
  await expect(page.locator('[data-test="import-text"]')).not.toHaveValue('');
  await page.locator('[data-test="import-target"]').selectOption('new');
  await page.screenshot({ path: 'shots/import-nodered-1-dialog.png' });
  await page.locator('[data-test="import-go"]').click();
  await expect(allNodes(page)).toHaveCount(4);
  await page.screenshot({ path: 'shots/import-nodered-2-canvas.png' });

  await topDeploy(page).click();
  await page.locator('[data-test="deploy-confirm"]').click();
  await expect(tabState(page)).toHaveText('saved');
  const flows = await deployed(request);
  const inject = flows.find((e) => e.type === 'inject' && e.payload === '');
  expect(inject, 'the imported Inject is deployed').toBeTruthy();
  const chg = flows.find((e) => e.type === 'change' && JSON.stringify(e.rules).includes('Hello World!'));
  expect(chg, 'the imported Change is deployed').toBeTruthy();

  await press(request, inject!.id);
  await expect(page.locator('body')).toContainText('Hello World!');
  await page.screenshot({ path: 'shots/import-nodered-3-ran.png' });
  expect(readFileSync(example, 'utf8')).toContain('Hello World!');
});

const vi = path.resolve(here, '..', '..', 'internal', 'vi', 'testdata', 'Get_Grayscale_Value.vi');

test('a LabVIEW VI imports through the dialog and lists what did not come across before anything is added', async ({ page, request }) => {
  await seed(request, threeNodes());
  await signIn(page);
  await page.locator('[data-test="import-open"]').click();
  await page.locator('[data-test="import-file"]').setInputFiles(vi);
  const list = page.locator('[data-test="import-unmapped"]');
  await expect(list).toContainText('primitive node(s)');
  await expect(list).toContainText('Color to RGB.vi');
  await page.screenshot({ path: 'shots/import-vi-1-dialog.png' });
  // Nothing has been added yet.
  await expect(allNodes(page)).toHaveCount(3);
  await page.locator('[data-test="import-go"]').click();
  // The VI comes in as a flow of its own, holding the note.
  await expect(allNodes(page)).toHaveCount(1);
  await page.screenshot({ path: 'shots/import-vi-2-canvas.png' });
});
