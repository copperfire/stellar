import React, { Component } from 'react';
import { BrowserRouter as Router, Route } from "react-router-dom";
import Constants from './Constants';
import styles from './App.module.scss';
import Header from './components/molecules/Header/Header';
import Button from './components/atoms/Button/Button';
import Bots from './components/screens/Bots/Bots';
import NewBot from './components/screens/NewBot/NewBot';
import version from './kelp-ops-api/version';
import quit from './kelp-ops-api/quit';
import Welcome from './components/molecules/Welcome/Welcome';

let baseUrl = function () {
  let origin = window.location.origin
  if (process.env.REACT_APP_API_PORT) {
    let parts = origin.split(":")
    return parts[0] + ":" + parts[1] + ":" + process.env.REACT_APP_API_PORT;
  }
  return origin;
}()

class App extends Component {
  constructor(props) {
    super(props);
    this.state = {
      version: "",
      kelp_errors: {},
      active_error: null, // { botName, level, errorList, index }
    };

    this.setVersion = this.setVersion.bind(this);
    this.quit = this.quit.bind(this);
    this.addError = this.addError.bind(this);
    this.removeError = this.removeError.bind(this);
    this.getErrors = this.getErrors.bind(this);
    this.setActiveBotError = this.setActiveBotError.bind(this);
    this.hideActiveError = this.hideActiveError.bind(this);
    this._asyncRequests = {};
  }

  componentDidMount() {
    this.setVersion()
  }

  setVersion() {
    var _this = this
    this._asyncRequests["version"] = version(baseUrl).then(resp => {
      if (!_this._asyncRequests["version"]) {
        // if it has been deleted it means we don't want to process the result
        return
      }

      delete _this._asyncRequests["version"];
      if (!resp.includes("error")) {
        _this.setState({ version: resp });
      } else {
        setTimeout(_this.setVersion, 30000);
      }
    });
  }

  quit() {
    var _this = this
    this._asyncRequests["quit"] = quit(baseUrl).then(resp => {
      if (!_this._asyncRequests["quit"]) {
        // if it has been deleted it means we don't want to process the result
        return
      }
      delete _this._asyncRequests["quit"];

      if (resp.status === 200) {
        window.close();
      }
    });
  }

  componentWillUnmount() {
    if (this._asyncRequests["version"]) {
      delete this._asyncRequests["version"];
    }
  }

  addError(backendError) {
    // TODO convert to hashID
    const ID = backendError.message

    // fetch object type from errors
    let kelp_errors = this.state.kelp_errors;

    if (!kelp_errors.hasOwnProperty(backendError.object_type)) {
      kelp_errors[backendError.object_type] = {};
    }
    let botErrors = kelp_errors[backendError.object_type];
    
    if (!botErrors.hasOwnProperty(backendError.object_name)) {
      botErrors[backendError.object_name] = {};
    }
    let namedError = botErrors[backendError.object_name];

    if (!namedError.hasOwnProperty(backendError.level)) {
      namedError[backendError.level] = {};
    }
    let levelErrors = namedError[backendError.level];

    if (!levelErrors.hasOwnProperty(ID)) {
      levelErrors[ID] = {
        occurrences: [],
        message: backendError.message,
      };
    }
    let idError = levelErrors[ID];

    // create new entry in list
    idError.occurrences.push(backendError.date);

    // trigger state change
    if (this.state.active_error === null) {
      this.setState({ "kelp_errors": kelp_errors });
    } else {
      let newState = {
        "kelp_errors": kelp_errors,
        "active_error": this.state.active_error,
      };
      // TODO add support to handle active errors that are not bot type errors
      if (
        backendError.object_type === Constants.ErrorType.bot &&
        backendError.object_name === this.state.active_error.botName &&
        backendError.level === this.state.active_error.level
      ) {
        // update activeErrors when it is affected (either errors or occurrences)
        newState.active_error.errorList = Object.values(levelErrors);
      }
      this.setState(newState);
    }
  }

  getErrors(object_type, object_name, level) {
    const kelp_errors = this.state.kelp_errors;

    if (!kelp_errors.hasOwnProperty(object_type)) {
      return [];
    }
    const botErrors = kelp_errors[object_type];
    
    if (!botErrors.hasOwnProperty(object_name)) {
      return [];
    }
    const namedError = botErrors[object_name];

    if (!namedError.hasOwnProperty(level)) {
      return [];
    }
    const levelErrors = namedError[level];

    // return as an array
    return Object.values(levelErrors);
  }

  removeError(object_type, object_name, level, errorID) {
    let kelp_errors = this.state.kelp_errors;
    let botErrors = kelp_errors[object_type];
    let namedError = botErrors[object_name];
    let levelErrors = namedError[level];
    
    // delete entry for error
    delete levelErrors[errorID];
    // bubble up
    if (Object.keys(levelErrors).length === 0) {
      delete namedError[level];
    }
    if (Object.keys(namedError).length === 0) {
      delete botErrors[object_name];
    }
    if (Object.keys(botErrors).length === 0) {
      delete kelp_errors[object_type];
    }

    let newState = {
      "kelp_errors": kelp_errors,
      "active_error": this.state.active_error,
    };
    // update the error that is now active accordingly
    newState.active_error.errorList = Object.values(levelErrors);
    const wasOnlyError = newState.active_error.errorList.length === 0;
    if (wasOnlyError) {
      newState.active_error = null;
    } else {
      const isLastError = newState.active_error.index > newState.active_error.errorList.length - 1;
      if (isLastError) {
        newState.active_error.index = newState.active_error.errorList.length - 1;
      } 
      // else leave index as-is since we just deleted the index and the new item will now be at the old index (delete in place)
    }
    // trigger state change
    this.setState(newState);
  }

  // TODO extend for non-bot type errors later
  setActiveBotError(botName, level, errorList, index) {
    this.setState({
      "active_error": {
        botName: botName,
        level: level,
        errorList: errorList,
        index: index,
      }
    });
  }

  hideActiveError() {
    this.setState({ "active_error": null });
  }

  render() {
    const enablePubnetBots = false;

    let banner = (<div className={styles.banner}>
      <Button
        className={styles.quit}
        size="small"
        onClick={this.quit}
      >
        Quit
      </Button>
      Kelp UI is only available on the Stellar Test Network
    </div>);

    const removeBotError = this.removeError.bind(this, Constants.ErrorType.bot);
    const getBotErrors = this.getErrors.bind(this, Constants.ErrorType.bot);

    return (
      <div>
        <div>{banner}</div>
        <Router>
          <Header version={this.state.version}/>
          <Route exact path="/"
            render={(props) => <Bots {...props} baseUrl={baseUrl} activeError={this.state.active_error} setActiveError={this.setActiveBotError} hideActiveError={this.hideActiveError} addError={this.addError} removeError={removeBotError} getErrors={getBotErrors}/>}
            />
          <Route exact path="/new"
            render={(props) => <NewBot {...props} baseUrl={baseUrl} enablePubnetBots={enablePubnetBots}/>}
            />
          <Route exact path="/edit"
            render={(props) => <NewBot {...props} baseUrl={baseUrl} enablePubnetBots={enablePubnetBots}/>}
            />
          <Route exact path="/details"
            render={(props) => <NewBot {...props} baseUrl={baseUrl} enablePubnetBots={enablePubnetBots}/>}
            />
        </Router>
        <Welcome quitFn={this.quit}/>
      </div>
    );
  }
}

export default App;
