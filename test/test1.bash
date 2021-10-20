#!/bin/bash
source ~/miniconda3/etc/profile.d/conda.sh
conda activate DataDiode
fName="test/test1.bash.gz"
cwd=$(pwd)
echo "${cwd}"
source ${cwd}/config/config.bash
source ${cwd}/config/functions.bash
t=$(getFileExtension "${fName}")
${DIODE_FILE_STEPS[$t]} "${fName}" "1"
