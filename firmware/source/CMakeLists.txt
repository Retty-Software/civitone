# Civitone input board (source): captures the codec's line or mic input and
# streams it to the controller over the control channel.
cmake_minimum_required(VERSION 3.16)
set(EXTRA_COMPONENT_DIRS ../components)
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(civitone-source)
