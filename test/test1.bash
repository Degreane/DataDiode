#!/bin/bash
fName="test/test1.bash.gz"
source /home/fbanna/projects/MOD/DataDiode/config/config.bash
source /home/fbanna/projects/MOD/DataDiode/config/functions.bash
t=$(getFileExtension "${fName}")
${DIODE_FILE_STEPS[$t]} "${fName}" "${t}"
