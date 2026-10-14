import * as VTable from '@visactor/vtable';
import * as VTableEditors from '@visactor/vtable-editors';
import * as VTablePlugins from '@visactor/vtable-plugins';
import ExcelJS from 'exceljs';

window.VTable = {
  ...VTable,
  editors: VTableEditors
};
window.VTablePlugins = VTablePlugins;
window.ExcelJS = ExcelJS;
