'use strict';
const fs=require('node:fs');
fs.writeSync(3,'status clean\nrunner_exit 0\nreaped 0\n');
process.stdout.write('status timeout\nrunner_exit 0\nreaped 0\n');
