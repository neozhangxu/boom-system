import { createApp, h, reactive } from 'vue';
import VxeUI from 'vxe-pc-ui';
import * as VxeTable from 'vxe-table';
import 'vxe-pc-ui/lib/style.css';
import 'vxe-table/lib/style.css';
import ExcelJS from 'exceljs';

window.BOMVendor = {
  createApp,
  h,
  reactive,
  VxeUI,
  VxeTable,
  ExcelJS
};
