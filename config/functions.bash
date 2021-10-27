# -------------------------------------------------------------- #
# functions.bash                                                 #
# Author: Faysal AL-Banna                                        #
# E-Mail: degreane@gmail.com                                     #
# -------------------------------------------------------------- #

## Declare Args as an associative array
declare -A ARGS

## getFileExtension <<<
## input : filename.ext
## return : Extension
## >>>

getFileExtension(){
    fName=$(${BASENAME} "${1}")
    fExt=$(echo ${fName//[0-9a-z_\ ]*\./} | ${TR} '[:upper:]' '[:lower:]')

    #`echo ${fName//[0-9a-z_\ ]*\./} | ${TR} '[:upper:]' '[:lower:]'`
    if [[ -z ${fExt} ]] ; then
        # echo false
        return 1
    fi
    # callF isIn DIODE_FILE_TYPES ${fExt} && return 0 || return 1
    # for ext in ${DIODE_FILE_TYPES[@]} ; do
    #     # echo "${ext} => ${fExt}"
    #     if [[ $fExt = ${ext} ]]; then
    #         # echo ${ext}
    #         return 0
    #     fi
    # done
    # # echo false
    # return 1
}

Document() {
    ## i shall use mraptor to check the file if it contains macros 
    fName="${ARGS[-F]}"
    ${DIODE_DOCUMENT_TOOL} "${fName}" 1>/dev/null
    ret=$?
    return ${ret}
}
Compressed() {
    ## get file name to be processed
    fName="${ARGS[-F]}"
    local level
    callF isIn ARGS "-L" && {
        level=${ARGS["-L"]}
    }|| {
        level=0
    }
    # level=$( expr ${2} + 0 )
    if [[ $level -gt $COMPRESSION_RECURSIVE ]]; then
        echo "Exiting Because Level is ${level}"
        return 1
    fi
    echo "Level Passed In is ${level}"
    tmpDir=$(mktemp -d)
    $DIODE_COMPRESS_TOOL extract --outdir "${tmpDir}" "${fName}"
    # for _file in $(${FIND} ${tmpDir} -type f -exec echo "'{}'" \;) ; do
    #     #find /tmp -type f -regextype posix-extended -regex  '.*\.gz|.*\.zip' -print
    #     echo " Executing Same FIle Again On ${_file}"
    #     bash ${0} -F "${_file}" -L $( expr ${level} + 1) || {
    #         return 1
    #     }
    # done
    while read -r _file ; do
        bash ${0} -F "${_file}" -L $( expr ${level} + 1 )
    done <<< $(${FIND} ${tmpDir} -type f -print )
    # ${FIND} ${tmpDir} -type f -exec bash test/test2.bash -F "'{}'" -L $( expr ${level} + 1 ) \; || {
    #     exit 1
    # }
}

getArgs() {
    while [[ ${#} -gt 0 ]] ; do
        arguement="${1}"
        # echo "Checking"
        # printf "%s\n" "${@}"
        # echo "arguement => ${arguement}"
        shift
        case "${arguement}" in
            -[a-zA-Z]* )
                if [[ ${#} -gt 0 ]]; then
                    value="${1}"
                    # echo "value => ${value}"
                    ARGS["${arguement}"]="${value}"
                    shift
                fi
        esac
        # echo
        # echo "#########################"
    done
#    parseArgs
}
parseArgs() {
    for ArgKey in "${!ARGS[@]}" ; do
        echo "(${ArgKey} ,${ARGS[${ArgKey}]})"
    done
}
isIn() {
    ## is in is a function that takes an array or an associative array
    ## here we pass the array/hash name and so ..
    # echo "${@}"
    if [[ ${#} -eq 2 ]]; then
        local -n AAName=${1}
        local varName=${1}
        local AAType=false
        # echo  "IS IN %s\n" ${!AAName[@]}
        if [[ "${AAName[@]@A}" =~ '-a' ]]; then
            AAType="Array"
            if [[ ${AAName[*]} =~ "${2}" ]]; then
                # echo true
                return 0
            else
                # echo false
                return 1
            fi
        elif [[ "${AAName[@]@A}" =~ '-A' ]]; then
            AAType="Hash"
            # echo "Hash"
            if [[ -z ${AAName[${2}]} ]]; then
                # echo false
                return 1
            else
                # echo true
                return 0
            fi

        else
            # echo "isIn: Input ${varName} is neither array nor hash"
            return 1
        fi
    else
        # echo "isIn: Input passed must be two variables"
        return 1
    fi
}
fileExists() {
    ## checks if the file submitted exists in the system
    ## returns true,1 on success
    ## false,0 on failure
    # echo "Checking File Exists ${ARGS[-F]}"
    if [[ -e "${ARGS[-F]}" ]]; then
        # echo "File ${1} Exists "
        return 0
    else
        # echo "File ${1} Does Not Exist "
        return 1
    fi
}
virusScan() {
    if [[ ${#} -eq 1 ]]; then
        "${CLAMAV}" -i "${ARGS[-F]// /\ }"
        scanResult=$?
        # echo "Scan Result ${scanResult}"
        [[ ${scanResult} =~ 'Infected files: 0' ]] && { echo true; return 0; } || { echo "false"; return 1; }
    else
        # echo "virusScan: File '${ARGS[-F]}' Not Found"
        return 1
    fi
}
chkErr() {
    if [[ ${1} -eq 0 ]] ; then
        return 0
    else
        # printf "%s: (Return Code %s)\n" ${2} ${1}
        return 1
    fi
}
callF() {
    ## Calls a function and checks for errors in the function
    # ${1} = function name
    # ${@:2} = function arguments
    [[ ${#} -gt 0 ]] && {
        funcName=${1}
        funcArguments=${@:2}
        # printf "Calling the Function %s \n--------\n" ${funcName}
        # printf "Args %s\n" ${funcArguments}
        # echo "------------------\n"
        ${funcName} ${funcArguments}
        local _res=$?
        chkErr ${_res} ${funcName}
        return $?
    } || {
        # echo "No Function Call "
        return 1
    }

}