#!/bin/bash
## initialize miniconda
source ~/miniconda3/etc/profile.d/conda.sh
conda activate DataDiode

## initialize scripts helpers and functions
cwd=$(pwd)
source ${cwd}/config/config.bash
source ${cwd}/config/functions.bash

getArgs ${@}
# callF funcMe 10 2
# exit 1
 callF isIn ARGS "-F"  && {
    callF fileExists ${ARGS["-F"]} && {
        # callF virusScan ${ARGS["-F"]} && {
            callF getFileExtension ${ARGS["-F"]} && {
                fName=$(${BASENAME} "${ARGS["-F"]}")
                fExt=$(echo ${fName//[0-9a-z_\ ]*\./} | ${TR} '[:upper:]' '[:lower:]')
                echo "Calling DIODE_FILE_TYPES with ${fExt}"
                callF isIn DIODE_FILE_TYPES ${fExt} && {
                    echo "Calling DIODE_FILE_STEPS with ${fExt}"
                    callF isIn DIODE_FILE_STEPS ${fExt} && {
                        
                        callF ${DIODE_FILE_STEPS[${fExt}]} ${ARGS["-F"]}
                    
                    } || {
                        echo "Resume Normal Scanning"
                    }
                }|| {
                    exit 1
                }
            }
        # }
    } || exit 1
 }





