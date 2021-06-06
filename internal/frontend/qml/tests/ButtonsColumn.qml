// Copyright (c) 2021 Proton Technologies AG
//
// This file is part of ProtonMail Bridge.
//
// ProtonMail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// ProtonMail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with ProtonMail Bridge.  If not, see <https://www.gnu.org/licenses/>.

import QtQuick.Layouts 1.12
import QtQuick 2.12
import QtQuick.Controls 2.12
import Proton 4.0

ColumnLayout {
    id: root
    property var colorScheme: parent.colorScheme

    property string textNormal: "Button"
    property string iconNormal: ""
    property string textDisabled: "Disabled"
    property string iconDisabled: ""
    property string textLoading: "Loading"
    property string iconLoading: ""
    property bool secondary: false

    Button {
        Layout.fillWidth: true

        Layout.minimumHeight: implicitHeight
        Layout.minimumWidth: implicitWidth

        text: root.textNormal
        icon.source: iconNormal
        secondary: root.secondary
    }


    Button {
        Layout.fillWidth: true

        Layout.minimumHeight: implicitHeight
        Layout.minimumWidth: implicitWidth

        text: root.textDisabled
        icon.source: iconDisabled
        secondary: root.secondary

        enabled: false
    }

    Button {
        Layout.fillWidth: true

        Layout.minimumHeight: implicitHeight
        Layout.minimumWidth: implicitWidth

        text: root.textLoading
        icon.source: iconLoading
        secondary: root.secondary

        loading: true
    }
}